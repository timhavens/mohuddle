//go:build !windows

package api

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/settings"
)

func TestChatGPTDefaultsReachOpenRoomsWithoutResettingUsageOrPauses(t *testing.T) {
	prefs, err := settings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, o, _, session := chatGPTService(t, nil)
	other, _, _, _ := chatGPTService(t, nil)
	s.ConfigureChatGPTLimits(func() chat.ChatGPTLimits { return prefs.ChatGPTLimits("inheriting") })
	other.ConfigureChatGPTLimits(func() chat.ChatGPTLimits { return prefs.ChatGPTLimits("pinned") })
	if err := prefs.SetChatGPTLimits("pinned", chat.DefaultChatGPTLimits()); err != nil {
		t.Fatal(err)
	}
	view := joinChatGPT(t, s, session)
	if _, err := s.ControlFollowUps("off"); err != nil {
		t.Fatal(err)
	}
	s.chatgptMu.Lock()
	s.chatgpt.exchanges = 7
	s.chatgptMu.Unlock()
	before, _ := o.Snapshot()
	personal := chat.ChatGPTLimits{Exchanges: 500, FollowUps: 500, FollowUpSeconds: 86400, RepeatedRequests: 3}
	if err := prefs.SetDefaultChatGPTLimits(&personal); err != nil {
		t.Fatal(err)
	}
	read := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID})
	if !read.OK {
		t.Fatal(read.Error)
	}
	after := read.Result.(ChatGPTView)
	if after.State.Limits != personal || after.State.ExchangesRemaining != 493 || after.FollowUps.Enabled || after.ParticipationID != view.ParticipationID {
		t.Fatalf("defaults lost state: %+v %+v", after.State, after.FollowUps)
	}
	room, _ := o.Snapshot()
	if !room.FollowUps.StartedAt.Equal(before.FollowUps.StartedAt) || room.FollowUps.Attempts != before.FollowUps.Attempts {
		t.Fatal("default update renewed allowance")
	}
	if got, _ := other.ChatGPTStatus(); got.Limits != chat.DefaultChatGPTLimits() {
		t.Fatal("default update changed pinned room")
	}
	if err := s.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	personal.Exchanges = 600
	if err := prefs.SetDefaultChatGPTLimits(&personal); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ChatGPTStatus(); got.Limits.Exchanges != 600 {
		t.Fatal("grant rotation lost settings resolver")
	}
}
