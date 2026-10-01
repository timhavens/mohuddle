package settings

import (
	"fmt"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/tunnel"
)

func (s *Store) ChatGPTLimits(roomKey string) chat.ChatGPTLimits {
	limits, _ := s.ChatGPTLimitsSource(roomKey)
	return limits
}

func (s *Store) ChatGPTLimitsSource(roomKey string) (chat.ChatGPTLimits, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limits, ok := s.config.ChatGPTRoomLimits[roomKey]; ok {
		return limits, "room override"
	}
	return s.defaultChatGPTLimitsLocked()
}

func (s *Store) defaultChatGPTLimitsLocked() (chat.ChatGPTLimits, string) {
	if s.config.ChatGPTDefaultLimits != nil {
		return *s.config.ChatGPTDefaultLimits, "personal default"
	}
	return chat.DefaultChatGPTLimits(), "built-in default"
}

func (s *Store) DefaultChatGPTLimits() chat.ChatGPTLimits {
	s.mu.Lock()
	defer s.mu.Unlock()
	limits, _ := s.defaultChatGPTLimitsLocked()
	return limits
}

// SetDefaultChatGPTLimits clears the personal default when limits is nil.
func (s *Store) SetDefaultChatGPTLimits(limits *chat.ChatGPTLimits) error {
	var value *chat.ChatGPTLimits
	if limits != nil {
		copy := *limits
		if err := copy.Validate(); err != nil {
			return err
		}
		value = &copy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.config.ChatGPTDefaultLimits
	s.config.ChatGPTDefaultLimits = value
	if err := s.saveLocked(); err != nil {
		s.config.ChatGPTDefaultLimits = previous
		return err
	}
	return nil
}

func (s *Store) InheritChatGPTLimits(roomKey string) error {
	if strings.TrimSpace(roomKey) == "" {
		return fmt.Errorf("ChatGPT limits require a room key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.config.ChatGPTRoomLimits[roomKey]
	delete(s.config.ChatGPTRoomLimits, roomKey)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.config.ChatGPTRoomLimits[roomKey] = previous
		}
		return err
	}
	return nil
}

func (s *Store) SetChatGPTLimits(roomKey string, limits chat.ChatGPTLimits) error {
	if strings.TrimSpace(roomKey) == "" {
		return fmt.Errorf("ChatGPT limits require a room key")
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.ChatGPTRoomLimits == nil {
		s.config.ChatGPTRoomLimits = make(map[string]chat.ChatGPTLimits)
	}
	previous, existed := s.config.ChatGPTRoomLimits[roomKey]
	// Explicit room choices stay pinned even when they equal today's defaults.
	s.config.ChatGPTRoomLimits[roomKey] = limits
	if err := s.saveLocked(); err != nil {
		if existed {
			s.config.ChatGPTRoomLimits[roomKey] = previous
		} else {
			delete(s.config.ChatGPTRoomLimits, roomKey)
		}
		return err
	}
	return nil
}

func (s *Store) ChatGPTProfile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.ChatGPTProfile == "" {
		return tunnel.DefaultProfile
	}
	return s.config.ChatGPTProfile
}

func (s *Store) SetChatGPTProfile(profile string) error {
	if !tunnel.ValidProfile(profile) {
		return fmt.Errorf("invalid tunnel profile name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.config.ChatGPTProfile
	s.config.ChatGPTProfile = profile
	if err := s.saveLocked(); err != nil {
		s.config.ChatGPTProfile = previous
		return err
	}
	return nil
}

// The key is the absolute room grant path, including its state directory. Auto
// access is an explicit local opt-in and never travels with a shared room file.
func (s *Store) ChatGPTAutoConnect(roomKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.ChatGPTAutoRooms[roomKey]
}

func (s *Store) SetChatGPTAutoConnect(roomKey string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.ChatGPTAutoRooms == nil {
		s.config.ChatGPTAutoRooms = make(map[string]bool)
	}
	previous := s.config.ChatGPTAutoRooms[roomKey]
	if enabled {
		s.config.ChatGPTAutoRooms[roomKey] = true
	} else {
		delete(s.config.ChatGPTAutoRooms, roomKey)
	}
	if err := s.saveLocked(); err != nil {
		if previous {
			s.config.ChatGPTAutoRooms[roomKey] = true
		} else {
			delete(s.config.ChatGPTAutoRooms, roomKey)
		}
		return err
	}
	return nil
}
