//go:build !windows

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/tunnel"
)

func TestChatGPTRosterTracksParticipationBelowLocalAgents(t *testing.T) {
	now := time.Now()
	model := Model{now: now, width: 120, activity: make(map[chat.Participant]participantActivity)}
	state := chat.ChatGPTState{Enabled: true, ExpiresAt: now.Add(time.Hour)}
	model.room.ChatGPT = &state
	for _, tc := range []struct {
		name, want                 string
		enabled, connected, paused bool
	}{
		{"waiting", "waiting for ChatGPT to join", true, false, false},
		{"connected", "connected", true, true, false},
		{"paused", "paused", true, true, true},
		{"disconnected", "disconnected", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.Enabled, state.Connected, state.Paused = tc.enabled, tc.connected, tc.paused
			state.LeaseUntil = now.Add(time.Minute)
			board := model.activityView()
			if !strings.Contains(board, tc.want) || strings.LastIndex(board, "CHATGPT") < strings.LastIndex(board, "COPILOT") {
				t.Fatalf("ChatGPT not last with current status: %s", board)
			}
		})
	}
	state.Enabled, state.Connected, state.Paused = true, true, false
	state.LeaseUntil = now.Add(-time.Second)
	if strings.Contains(model.chatGPTActivityLine(), "connected") {
		t.Fatal("expired lease shown connected")
	}
	state.ExpiresAt = now.Add(-time.Second)
	if !strings.Contains(model.chatGPTActivityLine(), "expired") {
		t.Fatal("expired grant hidden")
	}
}

func TestChatGPTRosterSeparatesParticipationAndTunnelHealth(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		transport  tunnel.State
		connected  bool
		paused     bool
		lease      time.Time
		wantPhase  activityPhase
		wantDetail string
	}{
		{"joined before health check", tunnel.Starting, true, false, now.Add(time.Minute), phaseIdle, "connected · ChatGPT website · tunnel health pending"},
		{"joined and healthy", tunnel.Ready, true, false, now.Add(time.Minute), phaseIdle, "connected · ChatGPT website"},
		{"healthy but not joined", tunnel.Ready, false, false, time.Time{}, phaseWaiting, "tunnel ready · waiting for ChatGPT to join"},
		{"expired lease during startup", tunnel.Starting, true, false, now.Add(-time.Second), phaseWaiting, "connecting · checking health"},
		{"paused during startup", tunnel.Starting, true, true, now.Add(time.Minute), phaseBlocked, "paused · /chatgpt resume · tunnel health pending"},
		{"joined but recovering", tunnel.Recovering, true, false, now.Add(time.Minute), phaseWaiting, "joined · reconnecting · checking health"},
		{"joined but failed", tunnel.Failed, true, false, now.Add(time.Minute), phaseError, "joined · tunnel error · checking health"},
		{"paused and failed", tunnel.Failed, true, true, now.Add(time.Minute), phaseError, "paused · tunnel error · checking health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := chat.ChatGPTState{Enabled: true, ExpiresAt: now.Add(time.Hour), Connected: tc.connected, Paused: tc.paused, LeaseUntil: tc.lease}
			activity := chatGPTConnectionActivity(&state, tunnel.Status{State: tc.transport, Detail: "checking health"}, now)
			if activity.Phase != tc.wantPhase || activity.Detail != tc.wantDetail {
				t.Fatalf("got %+v; want %s, %q", activity, tc.wantPhase, tc.wantDetail)
			}
		})
	}
}

func TestChatGPTLocalLifecycleAndAutoConnectWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, s, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := api.NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	defer service.RevokeChatGPT()
	path := filepath.Join(root, "chatgpt.json")
	service.ConfigureChatGPT(filepath.Join(root, "test.sock"), path, nil)
	prefs, err := settings.Open(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := tunnel.New(tunnel.Options{ProfileDir: root})
	defer manager.Close()
	model := New(o, s)
	model.ConfigureChatGPT(service)
	model.ConfigureChatGPTTunnel(manager, prefs, path)
	model.width, model.height, model.ready = 120, 40, true
	model.submit("/chatgpt auto on")
	if !prefs.ChatGPTAutoConnect(path) {
		t.Fatal("auto-connect opt-in lost")
	}
	updated, _ := model.Update(chatGPTAutoConnectMsg{})
	model = updated.(Model)
	state, _ := service.ChatGPTStatus()
	if !state.Enabled {
		t.Fatal("explicit remembered auto-connect did not authorize room")
	}
	o.Stop()
	model.submit("/chatgpt restart")
	state, _ = service.ChatGPTStatus()
	if !state.Paused {
		t.Fatal("restart unpaused participation")
	}
	view := model.View()
	firstLine, _, _ := strings.Cut(view, "\n")
	if strings.Contains(firstLine, "CHATGPT") || !strings.Contains(model.activityView(), "CHATGPT") {
		t.Fatal("ChatGPT was not moved from header to roster")
	}
	model.submit("/leave @chatgpt")
	state, _ = service.ChatGPTStatus()
	if state.Enabled || prefs.ChatGPTAutoConnect(path) || manager.Status().State != tunnel.Stopped {
		t.Fatal("leave failed to disable access, auto-connect or tunnel")
	}
	updated, _ = model.Update(chatGPTAutoConnectMsg{})
	model = updated.(Model)
	state, _ = service.ChatGPTStatus()
	if state.Enabled {
		t.Fatal("stale startup event overrode leave")
	}
	_, messages := o.Snapshot()
	if len(messages) != 0 {
		t.Fatal("private lifecycle commands entered shared history")
	}
	// A settings write failure must not let a queued startup event override an
	// explicit leave in the running room.
	model.submit("/chatgpt auto on")
	model.submit("/join @chatgpt")
	if err := os.Remove(prefs.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(prefs.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	model.submit("/leave @chatgpt")
	updated, _ = model.Update(chatGPTAutoConnectMsg{})
	model = updated.(Model)
	state, _ = service.ChatGPTStatus()
	if state.Enabled || !strings.Contains(noticesText(model.notices), "Could not save auto-connect OFF") {
		t.Fatal("leave lost to remembered startup after a persistence failure")
	}
}
