package settings

import (
	"fmt"

	"github.com/timhavens/mohuddle/internal/tunnel"
)

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
