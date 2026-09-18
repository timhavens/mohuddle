//go:build !windows

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

func TestChatGPTLocalControlsKeepCredentialsOutOfRoom(t *testing.T) {
	root := testutil.ShortTempDir(t)
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
	model := New(o, s)
	model.submit("/join @chatgpt")
	if !strings.Contains(noticesText(model.notices), "private local API") {
		t.Fatal("disabled API guidance missing")
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := api.NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.StartLocal(filepath.Join(root, "api.sock"), service, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	defer service.RevokeChatGPT()
	path := filepath.Join(root, "chatgpt.json")
	service.ConfigureChatGPT(server.Addr(), path, nil)
	model.ConfigureChatGPT(service)
	prefs, err := settings.Open(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	model.ConfigureChatGPTTunnel(nil, prefs, path)
	for _, command := range []string{"/chatgpt limits exchanges 64", "/chatgpt limits followups 50", "/chatgpt limits duration 2h", "/chatgpt limits repeats 4"} {
		model.submit(command)
	}
	want := chat.ChatGPTLimits{Exchanges: 64, FollowUps: 50, FollowUpSeconds: 7200, RepeatedRequests: 4}
	if got := prefs.ChatGPTLimits(path); got != want {
		t.Fatalf("limits commands: %+v", got)
	}
	state, _ := service.ChatGPTStatus()
	if state.Limits != want {
		t.Fatal("saved limits not applied")
	}
	for _, command := range []string{"/chatgpt limits exchanges 0", "/chatgpt limits followups -1", "/chatgpt limits duration 25h", "/chatgpt limits repeats 1", "/chatgpt limits duration 1.5s", "/chatgpt limits bogus 42", "/chatgpt limits reset extra"} {
		model.submit(command)
		if prefs.ChatGPTLimits(path) != want {
			t.Fatalf("invalid command changed settings: %s", command)
		}
	}
	reopened, err := settings.Open(prefs.Path())
	if err != nil {
		t.Fatal(err)
	}
	model.ConfigureChatGPTTunnel(nil, reopened, path)
	state, _ = service.ChatGPTStatus()
	if state.Limits != want {
		t.Fatal("startup did not load room limits")
	}
	model.notices = nil
	model.submit("/join @chatgpt")
	connection, err := api.ReadChatGPTConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noticesText(model.notices), "mohuddle chatgpt serve") || strings.Contains(noticesText(model.notices), "--connection") || strings.Contains(noticesText(model.notices), connection.Token) {
		t.Fatal("connection guidance missing or token printed")
	}
	model.submit("/agents")
	if !strings.Contains(noticesText(model.notices), "chatgpt") {
		t.Fatal("ChatGPT not listed in roster")
	}
	model.submit("/chatgpt on 25h")
	if !strings.Contains(noticesText(model.notices), "between 1m and 24h") {
		t.Fatal("invalid lifetime accepted")
	}
	model.submit("/chatgpt resume")
	if !strings.Contains(noticesText(model.notices), "64 further") {
		t.Fatal("resume notice did not use room limit")
	}
	model.submit("/chatgpt limits reset")
	state, _ = service.ChatGPTStatus()
	if state.Limits != chat.DefaultChatGPTLimits() {
		t.Fatal("reset did not apply defaults")
	}
	model.submit("/chatgpt status")
	if !strings.Contains(noticesText(model.notices), "32/32 exchanges remaining") {
		t.Fatal("remaining budget missing")
	}
	_, messages := o.Snapshot()
	if len(messages) != 0 {
		t.Fatal("local connection controls appeared in shared transcript")
	}
	for _, command := range []string{"/leave @chatgpt", "/leave @all"} {
		model.submit("/join @chatgpt")
		model.submit(command)
		state, _ := service.ChatGPTStatus()
		if state.Enabled {
			t.Fatalf("%s did not revoke access", command)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("private connection file retained")
		}
	}
}
