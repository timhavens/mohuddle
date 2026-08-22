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
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

const (
	roomFile          = "room.json"
	transcriptFile    = "messages.jsonl"
	composerFile      = "composer_history.json"
	attachmentsFolder = "attachments"
	maxComposerItems  = 200
)

type Store struct {
	root string
}

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
