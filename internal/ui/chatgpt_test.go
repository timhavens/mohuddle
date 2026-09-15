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
