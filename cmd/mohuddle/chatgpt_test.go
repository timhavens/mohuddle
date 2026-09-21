//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

func TestChatGPTCommandRejectsPublicListeningAndMissingGrants(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, args := range [][]string{nil, {"http"}, {"serve"}, {"serve", "--listen", ":8080"}, {"serve", "--connection", "relative.json"}, {"doctor", "--connection", "https://public.example/mcp"}, {"serve", "--room", "../room"}, {"serve", "--connection", "file.json", "--room", "room"}} {
		if err := runChatGPTCommand(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	if err := runChatGPTCommand([]string{"serve", "--help"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestChatGPTStdioHelperProcess(t *testing.T) {
	path := os.Getenv("MOHUDDLE_TEST_MCP_CONNECTION")
	stateDir := os.Getenv("MOHUDDLE_TEST_MCP_STATE_DIR")
	if path == "" && stateDir == "" {
		return
	}
	args := []string{"serve", "--connection", path}
	if stateDir != "" {
		args = []string{"serve", "--state-dir", stateDir}
	}
	if err := runChatGPTCommand(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func testChatGPTRoom(t *testing.T, root string) (*api.Service, *room.Orchestrator, string) {
	t.Helper()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, s, restartRosterAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := api.NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	local, err := api.StartLocal(filepath.Join(root, "api-"+r.ID+".sock"), service, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.RevokeChatGPT(); _ = local.Close(); _ = o.Close() })
	service.ConfigureChatGPT(local.Addr(), filepath.Join(root, "chatgpt-"+r.ID+".json"), nil)
	path, err := service.EnableChatGPT(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service, o, path
}

func TestChatGPTCommandSpeaksMCPOverStdio(t *testing.T) {
	root := testutil.ShortTempDir(t)
	service, o, path := testChatGPTRoom(t, root)
	var stdout, stderr bytes.Buffer
	if err := runChatGPTCommand([]string{"doctor", "--connection", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestChatGPTStdioHelperProcess$")
	command.Env = append(os.Environ(), "MOHUDDLE_TEST_MCP_STATE_DIR="+root)
	command.Stderr = &stderr
	client, err := mcp.NewClient(&mcp.Implementation{Name: "tunnel-test", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("stdio initialize: %v\n%s", err, stderr.String())
	}
	defer client.Close()
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 8 {
		t.Fatal("stdio tool discovery failed")
	}
	// Reproduce a host renewing access after the tunnel's MCP process started.
	if _, err := service.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_join", Arguments: map[string]any{"conversation_key": "stdio-test-conversation"}})
	if err != nil || result.IsError {
		t.Fatalf("stdio join failed: %v", err)
	}
	result, err = client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_publish", Arguments: map[string]any{"participation_id": result.StructuredContent.(map[string]any)["participation_id"], "operation_id": "stdio-post", "text": "Selected result from ChatGPT"}})
	if err != nil || result.IsError {
		t.Fatalf("stdio publish failed: %v", err)
	}
	_, messages := o.Snapshot()
	if len(messages) != 1 || messages[0].Author != chat.ChatGPT {
		t.Fatal("stdio contribution did not reach room")
	}
}

func TestChatGPTDiscoveryRequiresOneActiveAuthorizedRoom(t *testing.T) {
	parent := testutil.ShortTempDir(t)
	root := filepath.Join(parent, "mohuddle")
	t.Setenv("XDG_STATE_HOME", parent)
	if _, err := findChatGPTRoom(t.Context(), "", "", ""); err == nil {
		t.Fatal("discovery accepted an absent state directory")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("discovery created a state directory")
	}
	first, firstRoom, firstPath := testChatGPTRoom(t, root)
	second, secondRoom, secondPath := testChatGPTRoom(t, root)
	firstID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(firstPath), "chatgpt-"), ".json")
	secondID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(secondPath), "chatgpt-"), ".json")
	if _, err := findChatGPTRoom(t.Context(), "", "", ""); err == nil || !strings.Contains(err.Error(), "multiple authorized rooms") {
		t.Fatalf("discovery did not reject ambiguous rooms: %v", err)
	}
	if _, err := findChatGPTRoom(t.Context(), "", root, firstID); err != nil {
		t.Fatalf("explicit room selection failed: %v", err)
	}
	stale, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chatgpt-invalid.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongName, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chatgpt-wrong-room.json"), wrongName, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := findChatGPTRoom(t.Context(), "", "", ""); err != nil {
		t.Fatalf("discovery did not ignore stale or malformed grants: %v", err)
	}
	if _, err := findChatGPTRoom(t.Context(), "", root, "wrong-room"); err == nil {
		t.Fatal("room selection followed a descriptor for another room")
	}
	if _, err := findChatGPTRoom(t.Context(), "", root, secondID); err == nil {
		t.Fatal("explicit selection accepted a revoked grant")
	}
	for _, r := range []*room.Orchestrator{firstRoom, secondRoom} {
		state, messages := r.Snapshot()
		if state.Present(chat.ChatGPT) || len(messages) != 0 {
			t.Fatal("room discovery joined or changed a room")
		}
	}
	if err := first.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if _, err := findChatGPTRoom(t.Context(), "", root, ""); err == nil {
		t.Fatal("discovery accepted rooms without authorization")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := findChatGPTRoom(ctx, "", root, ""); err != context.Canceled {
		t.Fatalf("discovery ignored cancellation: %v", err)
	}
}
