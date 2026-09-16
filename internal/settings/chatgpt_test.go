package settings

import (
	"path/filepath"
	"testing"
)

func TestChatGPTSettingsRequireExplicitRoomOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ChatGPTProfile() != "mohuddle-chatgpt" || s.ChatGPTAutoConnect("/room-one") {
		t.Fatal("unsafe defaults")
	}
	if err := s.SetChatGPTProfile("../another-file"); err == nil {
		t.Fatal("invalid profile accepted")
	}
	if err := s.SetChatGPTProfile("private-profile"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChatGPTAutoConnect("/room-one", true); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ChatGPTProfile() != "private-profile" || !s.ChatGPTAutoConnect("/room-one") || s.ChatGPTAutoConnect("/room-two") {
		t.Fatal("room opt-in not scoped or not persisted")
	}
	if err := s.SetChatGPTAutoConnect("/room-one", false); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ChatGPTAutoConnect("/room-one") {
		t.Fatal("intentional stop persisted auto-connect")
	}
}
