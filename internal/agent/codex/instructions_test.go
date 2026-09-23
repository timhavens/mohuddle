package codex

import (
	"bufio"
	"context"
	"encoding/json"
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

func TestClientRefreshesHostInstructionsBetweenWorkflows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexInstructionHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_INSTRUCTION_HELPER", "1")
	client := New(Config{Binary: wrapper})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, step := range []struct {
		permissions chat.PermissionProfile
		workflow    string
		method      string
		turn        int
		updates     int
	}{
		{chat.PermissionReadOnly, "Review only.", "thread/start", 1, 0},
		{chat.PermissionReadOnly, "Review only.", "thread/start", 2, 0},
		{chat.PermissionFull, "Apply the approved edits.", "thread/start", 3, 1},
		{chat.PermissionFull, "Apply the approved edits.", "thread/start", 4, 1},
		{chat.PermissionFull, "Validate the saved edits.", "thread/start", 5, 2},
		{chat.PermissionReadOnly, "Review only.", "thread/start", 6, 3},
	} {
		settings := chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: step.permissions}
		prompt := agent.RoomProtocolPromptFor(chat.Codex, settings) + "\n" + step.workflow
		result, err := client.Run(ctx, agent.TurnRequest{
			Workspace: dir, SystemPrompt: prompt, Prompt: "Inspect the current instructions.", Settings: settings,
		}, func(agent.Event) {})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		var actual struct {
			Instructions string `json:"instructions"`
			Method       string `json:"method"`
			ThreadID     string `json:"thread_id"`
			Sandbox      string `json:"sandbox"`
			Turn         int    `json:"turn"`
			Updates      int    `json:"updates"`
		}
		if err := json.Unmarshal([]byte(result.Text), &actual); err != nil {
			t.Fatalf("step %d: decode result: %v", i, err)
		}
		if !strings.HasSuffix(actual.Instructions, prompt) || actual.Method != step.method || actual.Turn != step.turn || actual.Updates != step.updates ||
			actual.Sandbox != sandboxPolicy(step.permissions, nil)["type"] || result.SessionID != "instruction-thread" {
			t.Fatalf("step %d: stale instructions or session: %+v", i, actual)
		}
	}
}

func TestClientRefreshesResumedHostInstructionsBeforeWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint("reject=", reject), func(t *testing.T) {
			dir := t.TempDir()
			wrapper := filepath.Join(dir, "fake-codex")
			script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexInstructionHelperProcess -- \"$@\"\n", os.Args[0])
			if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MOHUDDLE_CODEX_INSTRUCTION_HELPER", "1")
			t.Setenv("MOHUDDLE_REJECT_HOST_UPDATE", fmt.Sprint(reject))
			turnMarker := filepath.Join(dir, "turn-started")
			t.Setenv("MOHUDDLE_INSTRUCTION_TURN_MARKER", turnMarker)
			client := New(Config{Binary: wrapper, SessionID: "instruction-thread"})
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := client.Run(ctx, agent.TurnRequest{
				Workspace: dir, SystemPrompt: "Apply the approved edits.", Prompt: "Execute the work.",
				Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionFull},
			}, func(agent.Event) {})
			if reject {
				if err == nil || !strings.Contains(err.Error(), "refresh Codex host instructions") {
					t.Fatalf("expected policy update error, got %v", err)
				}
				if _, err := os.Stat(turnMarker); !os.IsNotExist(err) {
					t.Fatal("work started despite rejected host instructions")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var actual struct {
				Instructions string `json:"instructions"`
				Method       string `json:"method"`
				ThreadID     string `json:"thread_id"`
				Updates      int    `json:"updates"`
			}
			if err := json.Unmarshal([]byte(result.Text), &actual); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(actual.Instructions, "Apply the approved edits.") || actual.Method != "thread/resume" || actual.ThreadID != "instruction-thread" || actual.Updates != 1 {
				t.Fatalf("resumed with stale host instructions: %s", result.Text)
			}
		})
	}
}

// Model the provider's thread-scoped instructions independently of per-turn
// sandbox overrides, so changing only filesystem permissions fails the test.
func TestCodexInstructionHelperProcess(t *testing.T) {
	if os.Getenv("MOHUDDLE_CODEX_INSTRUCTION_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var instructions, method, threadID string
	turn := 0
	updates := 0
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}})
		case "thread/start", "thread/resume":
			instructions, _ = request.Params["developerInstructions"].(string)
			if request.Method == "thread/resume" {
				instructions = "Retained historical developer instruction: do not modify files."
			}
			threadID, _ = request.Params["threadId"].(string)
			method = request.Method
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{
				"thread": map[string]any{"id": "instruction-thread"}, "model": "test-model", "reasoningEffort": "high",
			}})
		case "thread/inject_items":
			if os.Getenv("MOHUDDLE_REJECT_HOST_UPDATE") == "true" {
				_ = encoder.Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
				continue
			}
			items := request.Params["items"].([]any)
			item := items[0].(map[string]any)
			content := item["content"].([]any)[0].(map[string]any)
			if request.Params["threadId"] != "instruction-thread" || item["role"] != "developer" || content["type"] != "input_text" {
				os.Exit(3)
			}
			instructions = content["text"].(string)
			updates++
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}})
		case "turn/start":
			turn++
			if marker := os.Getenv("MOHUDDLE_INSTRUCTION_TURN_MARKER"); marker != "" {
				_ = os.WriteFile(marker, []byte("started"), 0o600)
			}
			policy := request.Params["sandboxPolicy"].(map[string]any)
			answer, _ := json.Marshal(map[string]any{
				"instructions": instructions, "method": method, "thread_id": threadID, "sandbox": policy["type"], "turn": turn, "updates": updates,
			})
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]any{"id": "instruction-turn"}}})
			_ = encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{
				"threadId": "instruction-thread", "turnId": "instruction-turn", "delta": string(answer),
			}})
			_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{
				"turn": map[string]any{"id": "instruction-turn", "status": "completed"},
			}})
		}
	}
	os.Exit(0)
}
