package store

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

const resetPendingFile = "reset_pending.json"

// ResetContext requires the caller to have closed every runtime using this room,
// while retaining its instance lock. The archive is durable before reset begins.
func (l *RoomLock) ResetContext() (string, error) {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := readRoomLock(filepath.Join(s.roomDir(l.roomID), roomLockFile))
	if err != nil {
		return "", err
	}
	if l.released || record.PID != os.Getpid() || !record.StartedAt.Equal(l.startedAt) {
		return "", fmt.Errorf("room lock ownership changed")
	}
	data, err := os.ReadFile(filepath.Join(s.roomDir(l.roomID), roomFile))
	if err != nil {
		return "", err
	}
	var old chat.Room
	if err := json.Unmarshal(data, &old); err != nil {
		return "", err
	}
	fresh := freshContext(old)
	archiveDir := filepath.Join(s.root, "archives", old.ID)
	if err := os.MkdirAll(archiveDir, 0700); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(archiveDir, "context-*.tar")
	if err != nil {
		return "", err
	}
	path := archive.Name()
	if err := archive.Chmod(0600); err != nil {
		archive.Close()
		return "", err
	}
	writer := tar.NewWriter(archive)
	err = filepath.WalkDir(s.roomDir(old.ID), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name, err := filepath.Rel(s.roomDir(old.ID), path)
		if err != nil {
			return err
		}
		if name == "." || name == roomLockFile {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("cannot archive non-regular room file %s", name)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(writer, f)
		return err
	})
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if syncErr := archive.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	pending, err := json.Marshal(fresh)
	if err != nil {
		return path, err
	}
	if err := s.writeRoomFile(old.ID, resetPendingFile, pending); err != nil {
		return path, err
	}
	return path, s.finishResetLocked(old.ID)
}

// Recovery is idempotent: a crash after the durable intent cannot replay old
// messages into the fresh provider sessions. The archive is never removed.
func (s *Store) finishResetLocked(id string) error {
	data, err := os.ReadFile(filepath.Join(s.roomDir(id), resetPendingFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// A second process must never finish a reset owned by a live host.
	if lock, lockErr := readRoomLock(filepath.Join(s.roomDir(id), roomLockFile)); lockErr == nil && lock.PID != os.Getpid() && processAlive(lock.PID) {
		return fmt.Errorf("room reset is in progress in process %d", lock.PID)
	}
	var fresh chat.Room
	if err := json.Unmarshal(data, &fresh); err != nil {
		return err
	}
	if fresh.ID != id {
		return fmt.Errorf("invalid room reset intent")
	}
	if err := s.writeRoomFile(id, transcriptFile, nil); err != nil {
		return err
	}
	if err := s.writeRoomFile(id, composerFile, []byte("[]\n")); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.roomDir(id), attachmentsFolder)); err != nil {
		return err
	}
	if err := s.SaveRoom(fresh); err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.roomDir(id), resetPendingFile))
}

func freshContext(old chat.Room) chat.Room {
	fresh := chat.NewRoom(old.ID, old.Workspace, old.MaxWaves, time.Now().UTC())
	fresh.CreatedAt = old.CreatedAt
	fresh.MaxTurns = old.MaxTurns
	fresh.Moderator, fresh.ModeratorPreference, fresh.ModeratorExplicit = old.Moderator, old.ModeratorPreference, old.ModeratorExplicit
	fresh.CorePolicy = old.CorePolicy
	fresh.Members, fresh.Grants, fresh.Settings = old.Members, old.Grants, old.Settings
	fresh.WorkflowMode, fresh.DelegationPolicy = old.WorkflowMode, old.DelegationPolicy
	fresh.StreamMode, fresh.ResponseStyle = old.StreamMode, old.ResponseStyle
	fresh.RoomPrompt, fresh.AgentPrompts = old.RoomPrompt, old.AgentPrompts
	fresh.Availability = old.Availability
	if f := old.FollowUps; f != nil {
		fresh.FollowUps = &chat.FollowUps{Enabled: f.Enabled, HostPaused: f.HostPaused, Revision: f.Revision + 1, StartedAt: f.StartedAt, Attempts: f.Attempts}
	}
	return fresh
}
