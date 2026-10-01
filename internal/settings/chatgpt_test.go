package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
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

func TestChatGPTRoomLimitsPersistAndLegacyConfigUsesNewDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":10}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.DefaultChatGPTLimits()
	if got := s.ChatGPTLimits("room-one"); got != want || got.Exchanges != 32 || got.FollowUps != 32 || got.FollowUpSeconds != 3600 {
		t.Fatalf("legacy defaults: %+v", got)
	}
	want.Exchanges, want.FollowUps, want.FollowUpSeconds, want.RepeatedRequests = 64, 50, 7200, 4
	if err := s.SetChatGPTLimits("room-one", want); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ChatGPTLimits("room-one") != want || s.ChatGPTLimits("room-two") != chat.DefaultChatGPTLimits() {
		t.Fatal("limits lost room isolation or persistence")
	}
	bad := want
	bad.Exchanges = 0
	if err := s.SetChatGPTLimits("room-one", bad); err == nil || s.ChatGPTLimits("room-one") != want {
		t.Fatal("invalid limits changed settings")
	}
	// Saving must roll back the in-memory preference if the atomic rename fails.
	s.path = t.TempDir()
	if err := s.SetChatGPTLimits("room-one", chat.DefaultChatGPTLimits()); err == nil || s.ChatGPTLimits("room-one") != want {
		t.Fatal("failed save changed active preference")
	}
	s.path = path
	if err := s.SetChatGPTLimits("room-one", chat.DefaultChatGPTLimits()); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil || s.ChatGPTLimits("room-one") != chat.DefaultChatGPTLimits() {
		t.Fatal("reset did not persist")
	}
}

func TestChatGPTRoomLimitsRejectInvalidSavedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":10,"chatgpt_room_limits":{"room":{"exchanges":-1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("invalid saved limits accepted")
	}
}

func TestPersonalChatGPTLimitsInheritancePersistenceAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	builtIn := chat.DefaultChatGPTLimits()
	personal := chat.ChatGPTLimits{Exchanges: 500, FollowUps: 500, FollowUpSeconds: 86400, RepeatedRequests: 3}
	if err := s.SetChatGPTLimits("pinned", builtIn); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultChatGPTLimits(&personal); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, source := s.ChatGPTLimitsSource("new-room"); got != personal || source != "personal default" {
		t.Fatalf("inherit: %+v %s", got, source)
	}
	if got, source := s.ChatGPTLimitsSource("pinned"); got != builtIn || source != "room override" {
		t.Fatalf("pin: %+v %s", got, source)
	}
	bad := personal
	bad.FollowUps = 1001
	if err := s.SetDefaultChatGPTLimits(&bad); err == nil || s.DefaultChatGPTLimits() != personal {
		t.Fatal("invalid default changed preferences")
	}
	s.path = t.TempDir()
	if err := s.SetDefaultChatGPTLimits(nil); err == nil || s.DefaultChatGPTLimits() != personal {
		t.Fatal("failed save cleared personal default")
	}
	if err := s.InheritChatGPTLimits("pinned"); err == nil || s.ChatGPTLimits("pinned") != builtIn {
		t.Fatal("failed save removed override")
	}
	s.path = path
	if err := s.InheritChatGPTLimits("pinned"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil || s.ChatGPTLimits("pinned") != personal {
		t.Fatal("inherit did not persist", err)
	}
	if err := s.SetDefaultChatGPTLimits(nil); err != nil {
		t.Fatal(err)
	}
	if got, source := s.ChatGPTLimitsSource("pinned"); got != builtIn || source != "built-in default" {
		t.Fatalf("reset: %+v %s", got, source)
	}
	if err := os.WriteFile(path, []byte(`{"version":11,"chatgpt_default_limits":{"exchanges":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("invalid persisted personal defaults accepted")
	}
}
