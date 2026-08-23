package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type AuditRecord struct {
	At           time.Time `json:"at"`
	ConnectionID string    `json:"connection_id"`
	Identity     string    `json:"identity,omitempty"`
	Remote       string    `json:"remote,omitempty"`
	Action       string    `json:"action"`
	RequestID    string    `json:"request_id,omitempty"`
	DeviceID     string    `json:"device_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	RoomID       string    `json:"room_id,omitempty"`
	Scopes       []Scope   `json:"scopes,omitempty"`
	Permission   string    `json:"permission_ceiling,omitempty"`
	Allowed      bool      `json:"allowed"`
	Error        string    `json:"error,omitempty"`
}

type AuditLog struct {
	path string
	mu   sync.Mutex
}

func NewAuditLog(path string) *AuditLog { return &AuditLog{path: path} }

func (a *AuditLog) Append(value AuditRecord) error {
	if a == nil || a.path == "" {
		return nil
	}
	value.At = time.Now().UTC()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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

// Recent returns the newest credential-free audit records in chronological
// order for trusted local status views. Tokens, keys, cookies, and request
// payloads are never fields in AuditRecord and therefore cannot be returned.
func (a *AuditLog) Recent(limit int) ([]AuditRecord, error) {
	if a == nil || a.path == "" || limit < 1 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	file, err := os.Open(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	values := make([]AuditRecord, 0, limit)
	for scanner.Scan() {
		var value AuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, fmt.Errorf("decode API audit record: %w", err)
		}
		if len(values) == limit {
			copy(values, values[1:])
			values[len(values)-1] = value
		} else {
			values = append(values, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
