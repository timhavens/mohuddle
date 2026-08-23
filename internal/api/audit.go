package api

import (
	"encoding/json"
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
