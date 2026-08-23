package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/speech"
)

const currentVersion = 4

type Config struct {
	Version                  int                                     `json:"version"`
	Defaults                 map[chat.Participant]chat.AgentSettings `json:"defaults,omitempty"`
	CoreDefaults             *chat.CorePolicy                        `json:"core,omitempty"`
	FullAccessAcknowledgedAt *time.Time                              `json:"full_access_acknowledged_at,omitempty"`
	ShowDetails              bool                                    `json:"show_details,omitempty"`
	Speech                   speech.Config                           `json:"speech,omitempty"`
}

type Store struct {
	mu     sync.Mutex
	path   string
	config Config
}

func DefaultPath() (string, error) {
	root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "mohuddle", "config.json"), nil
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("resolve config path: %w", err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	store := &Store{path: abs, config: Config{Version: currentVersion}}
	data, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &store.config); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if store.config.Version > currentVersion {
		return nil, fmt.Errorf("config version %d is newer than this MoHuddle supports", store.config.Version)
	}
	store.config.Version = currentVersion
	return store, nil
}

func BuiltIn(participant chat.Participant) chat.AgentSettings {
	return chat.AgentSettings{Permissions: participant.DefaultPermissions()}
}

func (s *Store) DefaultCorePolicy() chat.CorePolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.CoreDefaults == nil {
		return cloneCorePolicy(chat.BuiltInCorePolicy())
	}
	return cloneCorePolicy(s.config.CoreDefaults.WithDefaults())
}

func (s *Store) SetDefaultCorePolicy(value chat.CorePolicy) error {
	if err := value.Validate(); err != nil {
		return err
	}
	value = value.WithDefaults()
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := cloneCorePolicy(value)
	s.config.CoreDefaults = &copy
	return s.saveLocked()
}

func cloneCorePolicy(value chat.CorePolicy) chat.CorePolicy {
	value.Preferred = append([]chat.Participant(nil), value.Preferred...)
	value.Fallbacks = append([]chat.Participant(nil), value.Fallbacks...)
	return value
}

func (s *Store) Path() string { return s.path }

func (s *Store) Default(participant chat.Participant) chat.AgentSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.config.Defaults[participant]; ok {
		return NormalizeFor(participant, value)
	}
	return BuiltIn(participant)
}

func (s *Store) Effective(room chat.Room, participant chat.Participant) chat.AgentSettings {
	if value, ok := room.Settings[participant]; ok {
		return NormalizeFor(participant, value)
	}
	return s.Default(participant)
}

func (s *Store) FullAccessAcknowledged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.FullAccessAcknowledgedAt != nil
}

func (s *Store) DetailsVisible() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.ShowDetails
}

func (s *Store) SetDetailsVisible(visible bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.ShowDetails = visible
	return s.saveLocked()
}

func (s *Store) SpeechSettings() speech.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.Speech.WithDefaults()
}

func (s *Store) SetSpeechSettings(value speech.Config) error {
	value = value.WithDefaults()
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Speech = value
	return s.saveLocked()
}

func (s *Store) SetDefault(participant chat.Participant, value chat.AgentSettings) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid agent %q", participant)
	}
	value = NormalizeFor(participant, value)
	if err := ValidateFor(participant, value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.Permissions == chat.PermissionFull && s.config.FullAccessAcknowledgedAt == nil {
		return fmt.Errorf("full access requires acknowledgement")
	}
	if s.config.Defaults == nil {
		s.config.Defaults = make(map[chat.Participant]chat.AgentSettings, len(chat.Agents()))
	}
	s.config.Defaults[participant] = value
	return s.saveLocked()
}

func (s *Store) AcknowledgeFullAccess() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.config.FullAccessAcknowledgedAt = &now
	return s.saveLocked()
}

func Validate(value chat.AgentSettings) error {
	if !value.Permissions.Valid() {
		return fmt.Errorf("invalid permission profile %q", value.Permissions)
	}
	switch value.Effort {
	case "", "auto", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return nil
	default:
		return fmt.Errorf("invalid effort %q", value.Effort)
	}
}

func ValidateFor(participant chat.Participant, value chat.AgentSettings) error {
	if err := Validate(value); err != nil {
		return err
	}
	switch participant {
	case chat.Claude:
		if value.Effort == "none" || value.Effort == "minimal" || value.Effort == "ultra" {
			return fmt.Errorf("Claude effort must be auto, low, medium, high, xhigh, or max")
		}
	case chat.Agy:
		switch value.Effort {
		case "", "auto", "low", "medium", "high":
		default:
			return fmt.Errorf("AGY effort must be auto, low, medium, or high")
		}
	case chat.Copilot:
		if value.Effort == "ultra" {
			return fmt.Errorf("Copilot effort must be auto, none, minimal, low, medium, high, xhigh, or max")
		}
	}
	return nil
}

func Normalize(value chat.AgentSettings) chat.AgentSettings {
	value.Model = strings.TrimSpace(value.Model)
	value.Effort = strings.ToLower(strings.TrimSpace(value.Effort))
	return value.WithDefaults()
}

func NormalizeFor(participant chat.Participant, value chat.AgentSettings) chat.AgentSettings {
	if !value.Permissions.Valid() {
		value.Permissions = participant.DefaultPermissions()
	}
	return Normalize(value)
}

func (s *Store) saveLocked() error {
	s.config.Version = currentVersion
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
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
	return os.Rename(tmpName, s.path)
}
