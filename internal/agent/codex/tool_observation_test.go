package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestCommandObservationsUseFullOperationAndResults(t *testing.T) {
	observe := func(command, cwd, phase string, exit int, output string) *agent.ToolObservation {
		raw, _ := json.Marshal(map[string]any{"item": map[string]any{"id": "id", "type": "commandExecution", "command": command, "cwd": cwd, "exitCode": exit, "aggregatedOutput": output}})
		return ObservationFromItem(raw, "/workspace", agent.ToolPhase(phase))
	}
	a := observe("echo A", "/a", "started", 0, "")
	for _, v := range []*agent.ToolObservation{observe("echo a", "/a", "started", 0, ""), observe("echo A", "/b", "started", 0, "")} {
		if a.Operation == v.Operation {
			t.Fatal("distinct commands collapsed")
		}
	}
	end := observe("echo A", "/a", "completed", 1, "no match")
	if !end.Complete || end.Outcome != agent.ToolFailed || end.NonTransient || end.Result == "" || end.Operation != a.Operation {
		t.Fatalf("bad command outcome: %+v", end)
	}
	if end.Result == observe("echo A", "/a", "completed", 1, "different result").Result {
		t.Fatal("result changes lost")
	}
	unknown := ObservationFromItem(json.RawMessage(`{"item":{"type":"commandExecution","command":"pwd"}}`), "/w", agent.ToolStarted)
	if unknown.Complete {
		t.Fatal("missing ID claimed complete")
	}
	if itemBelongsToTurn(json.RawMessage(`{"threadId":"thread","turnId":"old"}`), "thread", "current") {
		t.Fatal("late turn event admitted")
	}
	for _, raw := range []string{`{"item":{"type":"webSearch"}}`, `{"item":{"type":"commandExecution","command":{}}}`, `broken`} {
		if !itemNeedsEvidenceBarrier(json.RawMessage(raw)) {
			t.Fatal("unknown work did not invalidate evidence")
		}
	}
	if itemNeedsEvidenceBarrier(json.RawMessage(`{"item":{"type":"reasoning"}}`)) {
		t.Fatal("reasoning is not a tool attempt")
	}
}

func TestMCPObservationClassifiesOnlyProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		error     string
		permanent bool
	}{{`{"code":-32602,"message":"invalid args"}`, true}, {`{"message":"temporary error"}`, false}} {
		raw := json.RawMessage(`{"item":{"id":"mcp","type":"mcpToolCall","server":"s","tool":"t","arguments":{},"error":` + tc.error + `}}`)
		v := ObservationFromItem(raw, "/w", agent.ToolCompleted)
		if !v.Complete || v.Outcome != agent.ToolFailed || v.NonTransient != tc.permanent || v.Result == "" {
			t.Fatalf("classification: %+v", v)
		}
	}
}

func TestClientParallelChecksEmitCompleteLifecycles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX test helper")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])), 0700); err != nil {
		t.Fatal(err)
	}
	frames, err := os.ReadFile("testdata/parallel_checks.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	t.Setenv("MOHUDDLE_CODEX_MCP_EVENTS", string(frames))
	c := New(Config{Binary: wrapper})
	defer c.Close()
	starts, ends := map[string]string{}, map[string]string{}
	_, err = c.Run(context.Background(), agent.TurnRequest{Prompt: "validate", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace}}, func(e agent.Event) {
		if e.Approval != nil {
			e.Approval.Response <- agent.ApproveOnce
		}
		v := e.ToolObservation
		if v == nil || len(v.InvocationID) < 6 || v.InvocationID[:6] != "check-" {
			return
		}
		if !v.Complete {
			t.Error("lost complete observation")
		}
		if v.Phase == agent.ToolStarted {
			starts[v.InvocationID] = v.Operation
		} else {
			ends[v.InvocationID] = v.Operation
			expected := agent.ToolSucceeded
			if v.InvocationID == "check-1" {
				expected = agent.ToolFailed
			}
			if v.Outcome != expected || v.NonTransient {
				t.Error("lost successful completion")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 4 || len(ends) != 4 {
		t.Fatalf("lifecycle starts=%d ends=%d", len(starts), len(ends))
	}
	for id, key := range starts {
		if ends[id] != key {
			t.Fatal("start/result identity mismatch")
		}
	}
}
