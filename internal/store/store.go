package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	roomFile           = "room.json"
	transcriptFile     = "messages.jsonl"
	composerFile       = "composer_history.json"
	attachmentsFolder  = "attachments"
	roomLockFile       = ".instance.lock"
	resumePointersFile = "resume_pointers.json"
	deletionAuditFile  = "room_deletions.jsonl"
	maxComposerItems   = 200
)

type Store struct {
	root string
	mu   sync.Mutex
}

type RoomLock struct {
	store     *Store
	roomID    string
	startedAt time.Time
	released  bool
}

type roomLockRecord struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type RoomDeleteInfo struct {
	ID           string    `json:"room_id"`
	Workspace    string    `json:"workspace"`
	MessageCount int       `json:"message_count"`
	DeletedAt    time.Time `json:"deleted_at"`
}

func (s *Store) Root() string { return s.root }

func DefaultStateDir() (string, error) {
	if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); state != "" {
		return filepath.Join(state, "mohuddle"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "mohuddle"), nil
}

func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return nil, fmt.Errorf("resolve state directory: %w", err)
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("secure state directory: %w", err)
	}
	return &Store{root: abs}, nil
}

func NewID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) Create(workspace string, maxWaves int) (chat.Room, error) {
	id, err := NewID()
	if err != nil {
		return chat.Room{}, err
	}
	room := chat.NewRoom(id, workspace, maxWaves, time.Now().UTC())
	if err := s.SaveRoom(room); err != nil {
		return chat.Room{}, err
	}
	return room, nil
}

func (s *Store) SaveRoom(room chat.Room) error {
	room.UpdatedAt = time.Now().UTC()
	dir := s.roomDir(room.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(room, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".room-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, roomFile))
}

func (s *Store) LoadRoom(id string) (chat.Room, error) {
	if err := validateID(id); err != nil {
		return chat.Room{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.roomDir(id), roomFile))
	if err != nil {
		return chat.Room{}, err
	}
	var room chat.Room
	if err := json.Unmarshal(data, &room); err != nil {
		return chat.Room{}, fmt.Errorf("decode room: %w", err)
	}
	if room.Sessions == nil {
		room.Sessions = map[chat.Participant]chat.AgentSession{}
	}
	if room.MaxWaves < 1 {
		room.MaxWaves = 3
	}
	return room, nil
}

func (s *Store) ListRooms() ([]chat.Room, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	rooms := make([]chat.Room, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateID(entry.Name()) != nil {
			continue
		}
		room, err := s.LoadRoom(entry.Name())
		if err == nil {
			rooms = append(rooms, room)
		}
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].UpdatedAt.After(rooms[j].UpdatedAt) })
	return rooms, nil
}

func (s *Store) RoomMessageCount(id string) (int, error) {
	messages, err := s.LoadMessages(id)
	return len(messages), err
}

func (s *Store) AcquireRoomLock(id string) (*RoomLock, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.roomDir(id), roomLockFile)
	for attempt := 0; attempt < 2; attempt++ {
		startedAt := time.Now().UTC()
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			record := roomLockRecord{PID: os.Getpid(), StartedAt: startedAt}
			data, marshalErr := json.Marshal(record)
			if marshalErr == nil {
				_, marshalErr = file.Write(append(data, '\n'))
			}
			if closeErr := file.Close(); marshalErr == nil {
				marshalErr = closeErr
			}
			if marshalErr != nil {
				_ = os.Remove(path)
				return nil, marshalErr
			}
			return &RoomLock{store: s, roomID: id, startedAt: startedAt}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		record, readErr := readRoomLock(path)
		if readErr == nil && processAlive(record.PID) {
			return nil, fmt.Errorf("room %s is open by live process %d since %s", id, record.PID, record.StartedAt.Local().Format(time.RFC3339))
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale room lock: %w", removeErr)
		}
	}
	return nil, fmt.Errorf("could not acquire room lock")
}

func readRoomLock(path string) (roomLockRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return roomLockRecord{}, err
	}
	var record roomLockRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return roomLockRecord{}, err
	}
	if record.PID <= 0 || record.StartedAt.IsZero() {
		return roomLockRecord{}, fmt.Errorf("invalid room lock")
	}
	return record, nil
}

func (l *RoomLock) Release() error {
	if l == nil || l.store == nil || l.released {
		return nil
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	path := filepath.Join(l.store.roomDir(l.roomID), roomLockFile)
	record, err := readRoomLock(path)
	if errors.Is(err, os.ErrNotExist) {
		l.released = true
		return nil
	}
	if err != nil {
		return err
	}
	if record.PID != os.Getpid() || !record.StartedAt.Equal(l.startedAt) {
		return fmt.Errorf("room lock ownership changed")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l.released = true
	return nil
}

// PeekRoomInUse reports whether a room lock belongs to a live process without
// modifying the lock. Listing callers use this so a read-only room listing does
// not become a stale-lock cleanup operation.
func (s *Store) PeekRoomInUse(id string) (bool, string, error) {
	if err := validateID(id); err != nil {
		return false, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := readRoomLock(filepath.Join(s.roomDir(id), roomLockFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if !processAlive(record.PID) {
		return false, "", nil
	}
	return true, fmt.Sprintf("live process %d since %s", record.PID, record.StartedAt.Local().Format(time.RFC3339)), nil
}

func (s *Store) RoomInUse(id string) (bool, string, error) {
	if err := validateID(id); err != nil {
		return false, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.roomDir(id), roomLockFile)
	record, err := readRoomLock(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err == nil && processAlive(record.PID) {
		return true, fmt.Sprintf("live process %d since %s", record.PID, record.StartedAt.Local().Format(time.RFC3339)), nil
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return false, "", removeErr
	}
	return false, "", nil
}

// DeleteRoom atomically removes a room from normal discovery before recursive
// deletion, so a partial failure cannot expose a half-deleted room.
func (s *Store) DeleteRoom(id string) (RoomDeleteInfo, error) {
	if err := validateID(id); err != nil {
		return RoomDeleteInfo{}, err
	}
	room, err := s.LoadRoom(id)
	if err != nil {
		return RoomDeleteInfo{}, err
	}
	count, err := s.RoomMessageCount(id)
	if err != nil {
		return RoomDeleteInfo{}, err
	}
	if inUse, reason, err := s.RoomInUse(id); err != nil {
		return RoomDeleteInfo{}, err
	} else if inUse {
		return RoomDeleteInfo{}, fmt.Errorf("room %s is currently open: %s", id, reason)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory := s.roomDir(id)
	relative, err := filepath.Rel(s.root, directory)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return RoomDeleteInfo{}, fmt.Errorf("room path escapes the state root")
	}
	// Claim the lock path before rename to close the cross-process race between
	// the liveness check above and removing the room from discovery.
	lockPath := filepath.Join(directory, roomLockFile)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return RoomDeleteInfo{}, fmt.Errorf("room %s became open while deletion was being confirmed", id)
	}
	record, _ := json.Marshal(roomLockRecord{PID: os.Getpid(), StartedAt: time.Now().UTC()})
	if _, err := lock.Write(append(record, '\n')); err != nil {
		lock.Close()
		_ = os.Remove(lockPath)
		return RoomDeleteInfo{}, err
	}
	if err := lock.Close(); err != nil {
		_ = os.Remove(lockPath)
		return RoomDeleteInfo{}, err
	}
	suffix, err := NewID()
	if err != nil {
		return RoomDeleteInfo{}, err
	}
	moved := filepath.Join(s.root, ".deleting-"+id+"-"+suffix)
	if err := os.Rename(directory, moved); err != nil {
		return RoomDeleteInfo{}, err
	}
	if err := os.RemoveAll(moved); err != nil {
		return RoomDeleteInfo{}, fmt.Errorf("room moved aside to %s but cleanup failed: %w", filepath.Base(moved), err)
	}
	info := RoomDeleteInfo{ID: id, Workspace: room.Workspace, MessageCount: count, DeletedAt: time.Now().UTC()}
	if err := s.clearResumePointerLocked(id); err != nil {
		return info, err
	}
	if err := s.appendDeletionAuditLocked(info); err != nil {
		return info, err
	}
	return info, nil
}

func (s *Store) SetResumePointer(workspace, id string) error {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "." || workspace == "" {
		return fmt.Errorf("workspace is required")
	}
	if id != "" {
		if err := validateID(id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pointers, err := s.loadResumePointersLocked()
	if err != nil {
		return err
	}
	pointers[workspace] = id
	return s.writeRootJSONLocked(resumePointersFile, pointers)
}

func (s *Store) ResumePointer(workspace string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pointers, err := s.loadResumePointersLocked()
	if err != nil {
		return "", false, err
	}
	id, ok := pointers[filepath.Clean(strings.TrimSpace(workspace))]
	return id, ok, nil
}

func (s *Store) loadResumePointersLocked() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(s.root, resumePointersFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode resume pointers: %w", err)
	}
	return result, nil
}

func (s *Store) clearResumePointerLocked(id string) error {
	pointers, err := s.loadResumePointersLocked()
	if err != nil {
		return err
	}
	changed := false
	for workspace, current := range pointers {
		if current == id {
			pointers[workspace] = ""
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.writeRootJSONLocked(resumePointersFile, pointers)
}

func (s *Store) writeRootJSONLocked(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".root-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(s.root, name))
}

func (s *Store) appendDeletionAuditLocked(info RoomDeleteInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.root, deletionAuditFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Store) AppendMessage(roomID string, message chat.Message) error {
	if err := validateID(roomID); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	dir := s.roomDir(roomID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, transcriptFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Store) LoadMessages(roomID string) ([]chat.Message, error) {
	if err := validateID(roomID); err != nil {
		return nil, err
	}
	path := filepath.Join(s.roomDir(roomID), transcriptFile)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var messages []chat.Message
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var message chat.Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			// A process can be interrupted between append and newline. Only a
			// malformed final record is recoverable; corruption in the middle is not.
			if !scanner.Scan() {
				return messages, nil
			}
			return nil, fmt.Errorf("decode transcript line %d: %w", line, err)
		}
		messages = append(messages, message)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) LoadComposerHistory(roomID string) ([]chat.ComposerHistoryEntry, error) {
	if err := validateID(roomID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.roomDir(roomID), composerFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []chat.ComposerHistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode composer history: %w", err)
	}
	if len(entries) > maxComposerItems {
		entries = entries[len(entries)-maxComposerItems:]
	}
	return entries, nil
}

func (s *Store) SaveComposerHistory(roomID string, entries []chat.ComposerHistoryEntry) error {
	if err := validateID(roomID); err != nil {
		return err
	}
	if len(entries) > maxComposerItems {
		entries = entries[len(entries)-maxComposerItems:]
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.writeRoomFile(roomID, composerFile, data)
}

func (s *Store) SaveAttachment(roomID string, value chat.Attachment, data []byte) (chat.Attachment, error) {
	if err := validateID(roomID); err != nil {
		return chat.Attachment{}, err
	}
	if value.Kind != chat.AttachmentImage {
		return chat.Attachment{}, fmt.Errorf("unsupported attachment kind %q", value.Kind)
	}
	id, err := NewID()
	if err != nil {
		return chat.Attachment{}, err
	}
	dir := filepath.Join(s.roomDir(roomID), attachmentsFolder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return chat.Attachment{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return chat.Attachment{}, err
	}
	path := filepath.Join(dir, id+".png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return chat.Attachment{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return chat.Attachment{}, err
	}
	value.ID = id
	value.Path = path
	value.Size = int64(len(data))
	if strings.TrimSpace(value.Name) == "" {
		value.Name = "image.png"
	}
	return value, nil
}

func (s *Store) writeRoomFile(roomID, name string, data []byte) error {
	dir := s.roomDir(roomID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

func (s *Store) roomDir(id string) string {
	return filepath.Join(s.root, id)
}

func validateID(id string) error {
	if len(id) != 24 {
		return fmt.Errorf("invalid room id")
	}
	_, err := hex.DecodeString(id)
	if err != nil {
		return fmt.Errorf("invalid room id")
	}
	return nil
}
