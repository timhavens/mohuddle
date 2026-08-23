//go:build !windows

package api

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

func TestLocalServerAuthenticatesAndStreamsEvents(t *testing.T) {
	service, controller, session := testService(t, ClientLocal, ScopeObserve, ScopeParticipate, ScopeAdminister)
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(socketDir, "api.sock")
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	server, err := StartLocal(path, service, NewAuditLog(auditPath))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode=%v", info.Mode())
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	encoder := json.NewEncoder(connection)
	decoder := json.NewDecoder(connection)
	credential := service.credentials.Entries[0]
	if err := encoder.Encode(request(t, "hello", "hello", HelloRequest{ClientID: "wire-client", Token: credential.Token})); err != nil {
		t.Fatal(err)
	}
	var hello Response
	if err := decoder.Decode(&hello); err != nil || !hello.OK {
		t.Fatalf("hello=%+v err=%v", hello, err)
	}
	if err := encoder.Encode(request(t, "join", "room.join", JoinRoomRequest{RoomID: controller.room.ID})); err != nil {
		t.Fatal(err)
	}
	var joined Response
	if err := decoder.Decode(&joined); err != nil || !joined.OK {
		t.Fatalf("join=%+v err=%v", joined, err)
	}
	subscribe := Request{Version: Version, ID: "subscribe", Type: "events.subscribe", RoomID: controller.room.ID}
	if err := encoder.Encode(subscribe); err != nil {
		t.Fatal(err)
	}
	var subscribed Response
	if err := decoder.Decode(&subscribed); err != nil || !subscribed.OK {
		t.Fatalf("subscribe=%+v err=%v", subscribed, err)
	}
	controller.events <- room.Event{Type: room.EventMessage, Message: &chat.Message{ID: "message", Sequence: 2, Author: chat.Codex, Kind: chat.MessageText, Text: "streamed"}}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var event Event
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Version != Version || event.RoomID != controller.room.ID || event.Payload.Message == nil || event.Payload.Message.Text != "streamed" {
		t.Fatalf("event=%+v", event)
	}
	if event.Route.OriginInstanceID != service.InstanceID() || event.Route.OriginClientID == "" {
		t.Fatalf("route=%+v", event.Route)
	}
	if session.Identity == "" {
		t.Fatal("test session identity is empty")
	}
	connection.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remained after close: %v", err)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"action":"hello"`) || !strings.Contains(string(audit), `"action":"events.subscribe"`) {
		t.Fatalf("audit=%s", audit)
	}
}

func TestLocalServerCloseInterruptsIdleConnections(t *testing.T) {
	service, _, _ := testService(t, ClientLocal, ScopeObserve)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := StartLocal(filepath.Join(dir, "api.sock"), service, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("unix", server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() { done <- server.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server close blocked on an idle connection")
	}
}

func TestLocalServerRejectsSocketInSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	service, _, _ := testService(t, ClientLocal, ScopeObserve)
	if _, err := StartLocal(filepath.Join(dir, "api.sock"), service, nil); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("error=%v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("shared parent permissions changed to %o", info.Mode().Perm())
	}
}
