//go:build !windows

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
)

func TestEnsureChatGPTPreservesAuthorizationAndParticipation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, s, chatGPTTestAgent{})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	credentials, err := LoadOrCreateCredentials(CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "chatgpt.json")
	service.ConfigureChatGPT(filepath.Join(root, "test.sock"), path, nil)
	defer service.RevokeChatGPT()
	if _, err := service.EnsureChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var grant ChatGPTConnection
	if err := json.Unmarshal(before, &grant); err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(HelloRequest{ClientID: "test", Token: grant.Token})
	if err != nil {
		t.Fatal(err)
	}
	// Use the real service/controller, without opening a network listener.
	result := service.Handle(t.Context(), session, request(t, "join", "chatgpt.join", ChatGPTJoinRequest{ClientKey: "one-chat"})).Response
	if !result.OK {
		t.Fatal(result.Error)
	}
	participation := result.Result.(ChatGPTView).ParticipationID
	service.chatgptMu.Lock()
	service.chatgpt.exchanges = 5
	service.chatgptMu.Unlock()
	o.Stop()
	for i := 0; i < 3; i++ {
		if _, err := service.EnsureChatGPT(8 * time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(path)
	state, _ := service.ChatGPTStatus()
	if string(before) != string(after) || !state.Paused || state.ExchangesRemaining != 3 || service.chatgpt.participation != participation {
		t.Fatalf("idempotent join reset state: %+v", state)
	}
	if _, err := service.EnsureChatGPT(25 * time.Hour); err == nil {
		t.Fatal("invalid TTL accepted with existing grant")
	}
	if err := service.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	newGrant, _ := os.ReadFile(path)
	if string(newGrant) == string(before) {
		t.Fatal("revoked credentials reused")
	}
}
