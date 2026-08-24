package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestClientRunAppServerLifecycleAndApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	imagePath := filepath.Join(dir, "image.png")
	t.Setenv("MOHUDDLE_EXPECTED_IMAGE", imagePath)
	client := New(Config{Binary: wrapper})
	defer client.Close()
	var events []agent.Event
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "hello", Attachments: []chat.Attachment{{Kind: chat.AttachmentImage, Path: imagePath}}, Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, SystemPrompt: "system",
		Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace},
	}, func(event agent.Event) {
		events = append(events, event)
		if event.Approval != nil {
			event.Approval.Response <- agent.ApproveOnce
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello from codex" || result.Done || result.SessionID != "codex-thread" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.Delegates) != 1 || result.Delegates[0].Participant != chat.Participant("codex-1") || result.Delegates[0].Task != "inspect the parser" {
		t.Fatalf("completed-item control marker was not preserved: %+v", result.Delegates)
	}
	seenApproval := false
	for _, event := range events {
		seenApproval = seenApproval || event.Type == agent.EventApproval
	}
	if !seenApproval {
		t.Fatal("approval event was not surfaced")
	}
}

func TestClientRestartsAndRetriesStalledTurnStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	stallMarker := filepath.Join(dir, "stalled-once")
	t.Setenv("MOHUDDLE_STALL_FIRST_TURN_START", stallMarker)
	client := New(Config{Binary: wrapper, Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace})
	client.turnStartTimeout = 100 * time.Millisecond
	defer client.Close()
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "retry", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, SystemPrompt: "system",
		Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace},
	}, func(event agent.Event) {
		if event.Approval != nil {
			event.Approval.Response <- agent.ApproveOnce
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello from codex" || result.Done || len(result.Delegates) != 1 {
		t.Fatalf("unexpected retried result: %+v", result)
	}
	if _, err := os.Stat(stallMarker); err != nil {
		t.Fatalf("first turn/start did not exercise the stall: %v", err)
	}
}

func TestCompletedAgentMessageIgnoresCommentaryPhase(t *testing.T) {
	text, turnID := completedAgentMessage(json.RawMessage(`{"turnId":"turn","item":{"type":"agentMessage","phase":"commentary","text":"not the final answer"}}`))
	if text != "" || turnID != "turn" {
		t.Fatalf("commentary extraction text=%q turn=%q", text, turnID)
	}
}

func TestClientListsCodexModels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	client := New(Config{Binary: wrapper})
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-test" || models[0].Name != "GPT Test" || !models[0].Default || len(models[0].Efforts) != 2 {
		t.Fatalf("models=%+v", models)
	}
}

func TestClientNoToolsTurnIsIsolatedAndFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	t.Setenv("MOHUDDLE_ORIGINAL_WORKSPACE", dir)
	client := New(Config{Binary: wrapper})
	defer client.Close()
	_, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "route", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir},
		SystemPrompt: "system", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly},
		Ephemeral: true, NoTools: true,
	}, func(agent.Event) {})
	if err == nil || !strings.Contains(err.Error(), "attempted tool use during a no-tools turn") {
		t.Fatalf("expected fail-closed no-tools error, got %v", err)
	}
}

func TestCodexHelperProcess(t *testing.T) {
	if os.Getenv("MOHUDDLE_CODEX_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		method, _ := request["method"].(string)
		id, hasID := request["id"]
		switch method {
		case "initialize":
			params := request["params"].(map[string]any)
			clientInfo := params["clientInfo"].(map[string]any)
			if clientInfo["name"] != "mohuddle" || clientInfo["title"] != "MoHuddle" {
				os.Exit(4)
			}
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"userAgent": "fake"}})
		case "initialized":
		case "model/list":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"data": []map[string]any{{
				"id": "gpt-test", "model": "gpt-test", "displayName": "GPT Test", "isDefault": true,
				"supportedReasoningEfforts": []map[string]any{{"reasoningEffort": "medium"}, {"reasoningEffort": "high"}},
			}}}})
		case "thread/start":
			params := request["params"].(map[string]any)
			if _, noTools := params["dynamicTools"]; noTools {
				roots, rootsOK := params["runtimeWorkspaceRoots"].([]any)
				environments, environmentOK := params["environments"].([]any)
				if params["sandbox"] != "read-only" || params["cwd"] == os.Getenv("MOHUDDLE_ORIGINAL_WORKSPACE") || !rootsOK || len(roots) != 0 || !environmentOK || len(environments) != 0 {
					os.Exit(6)
				}
				_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "codex-private-thread"}}})
				continue
			}
			if params["sandbox"] != "workspace-write" || params["approvalPolicy"] != "never" || params["model"] != "test-model" {
				_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": -1, "message": "missing sandbox"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "codex-thread"}}})
		case "turn/start":
			params := request["params"].(map[string]any)
			if marker := os.Getenv("MOHUDDLE_STALL_FIRST_TURN_START"); marker != "" {
				if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
					if err := os.WriteFile(marker, []byte("stalled"), 0o600); err != nil {
						os.Exit(12)
					}
					continue
				}
			}
			policy := params["sandboxPolicy"].(map[string]any)
			if _, noTools := params["environments"]; noTools {
				roots, rootsOK := params["runtimeWorkspaceRoots"].([]any)
				if policy["type"] != "readOnly" || params["cwd"] == os.Getenv("MOHUDDLE_ORIGINAL_WORKSPACE") || !rootsOK || len(roots) != 0 {
					os.Exit(7)
				}
				_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "codex-private-turn"}}})
				_ = encoder.Encode(map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]any{"type": "commandExecution", "command": "pwd", "status": "inProgress"}}})
				continue
			}
			if params["model"] != "test-model" || params["effort"] != "high" || params["approvalPolicy"] != "never" || policy["type"] != "workspaceWrite" || policy["networkAccess"] != false {
				os.Exit(5)
			}
			if expected := os.Getenv("MOHUDDLE_EXPECTED_IMAGE"); expected != "" {
				input, ok := params["input"].([]any)
				if !ok || len(input) != 2 {
					os.Exit(10)
				}
				image, ok := input[1].(map[string]any)
				if !ok || image["type"] != "localImage" || image["path"] != expected {
					os.Exit(11)
				}
			}
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "codex-turn"}}})
			_ = encoder.Encode(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "codex-thread", "turnId": "codex-turn", "itemId": "item", "command": "go test ./..."}})
		case "":
			if hasID && fmt.Sprint(id) == "900" {
				result := request["result"].(map[string]any)
				if result["decision"] != "accept" {
					os.Exit(3)
				}
				_ = encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "codex-thread", "turnId": "codex-turn", "delta": "hello from codex"}})
				_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{
					"threadId": "codex-thread", "turnId": "codex-turn",
					"item": map[string]any{
						"id": "answer", "type": "agentMessage", "phase": "final_answer",
						"text": "hello from codex\n<!-- mohuddle:{\"done\":false,\"delegates\":[{\"participant\":\"codex-1\",\"task\":\"inspect the parser\"}]} -->",
					},
				}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"thread": map[string]any{"id": "codex-thread"}, "turn": map[string]any{"id": "codex-turn", "status": "completed"}}})
				os.Exit(0)
			}
		case "turn/interrupt":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		}
	}
	os.Exit(0)
}

func TestPermissionProfilesMapToCodexSandbox(t *testing.T) {
	tests := []struct {
		profile chat.PermissionProfile
		mode    string
		kind    string
		offline bool
	}{
		{chat.PermissionReadOnly, "read-only", "readOnly", true},
		{chat.PermissionWorkspace, "workspace-write", "workspaceWrite", true},
		{chat.PermissionFull, "danger-full-access", "dangerFullAccess", false},
	}
	for _, test := range tests {
		if got := sandboxMode(test.profile); got != test.mode {
			t.Errorf("sandboxMode(%q)=%q want %q", test.profile, got, test.mode)
		}
		policy := sandboxPolicy(test.profile, []string{"/workspace"})
		if got := policy["type"]; got != test.kind {
			t.Errorf("sandboxPolicy(%q) type=%q want %q", test.profile, got, test.kind)
		}
		if got, present := policy["networkAccess"]; test.offline && (!present || got != false) {
			t.Errorf("sandboxPolicy(%q) networkAccess=%v present=%v, want false", test.profile, got, present)
		} else if !test.offline && present {
			t.Errorf("sandboxPolicy(%q) unexpectedly restricts full-profile network: %v", test.profile, got)
		}
	}
}
