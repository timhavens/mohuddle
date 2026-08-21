package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
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
	client := New(Config{Binary: wrapper})
	defer client.Close()
	var events []agent.Event
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "hello", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, SystemPrompt: "system",
	}, func(event agent.Event) {
		events = append(events, event)
		if event.Approval != nil {
			event.Approval.Response <- agent.ApproveOnce
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello from codex" || !result.Done || result.SessionID != "codex-thread" {
		t.Fatalf("unexpected result: %+v", result)
	}
	seenApproval := false
	for _, event := range events {
		seenApproval = seenApproval || event.Type == agent.EventApproval
	}
	if !seenApproval {
		t.Fatal("approval event was not surfaced")
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
		case "thread/start":
			params := request["params"].(map[string]any)
			if params["sandbox"] != "workspace-write" {
				_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": -1, "message": "missing sandbox"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": "codex-thread"}}})
		case "turn/start":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "codex-turn"}}})
			_ = encoder.Encode(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "codex-thread", "turnId": "codex-turn", "itemId": "item", "command": "go test ./..."}})
		case "":
			if hasID && fmt.Sprint(id) == "900" {
				result := request["result"].(map[string]any)
				if result["decision"] != "accept" {
					os.Exit(3)
				}
				_ = encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "codex-thread", "turnId": "codex-turn", "delta": "hello from codex\n<!-- mohuddle:{\"done\":true} -->"}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"thread": map[string]any{"id": "codex-thread"}, "turn": map[string]any{"id": "codex-turn", "status": "completed"}}})
				os.Exit(0)
			}
		case "turn/interrupt":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{}})
		}
	}
	os.Exit(0)
}
