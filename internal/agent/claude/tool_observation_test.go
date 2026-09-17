package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
)

func TestClaudeToolResultsRetainIDsAndExactInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	script := `#!/bin/sh
cat >/dev/null
cat <<'EVENTS'
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"mcp__graph__search","input":{"id":9007199254740992}},{"type":"tool_use","id":"b","name":"mcp__graph__search","input":{"id":9007199254740993}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"failed"},{"type":"tool_result","tool_use_id":"a","content":"ok"}]}}
{"type":"result","result":"done","is_error":false}
EVENTS
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Binary: binary})
	defer c.Close()
	starts, ends := map[string]agent.ToolObservation{}, map[string]agent.ToolObservation{}
	_, err := c.Run(context.Background(), agent.TurnRequest{Prompt: "test", Workspace: dir}, func(e agent.Event) {
		if v := e.ToolObservation; v != nil {
			if v.Phase == agent.ToolStarted {
				starts[v.InvocationID] = *v
			} else {
				ends[v.InvocationID] = *v
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 2 || len(ends) != 2 || starts["a"].Operation == starts["b"].Operation {
		t.Fatal("distinct inputs collapsed")
	}
	for id, v := range starts {
		if !v.Complete || v.Operation != ends[id].Operation || ends[id].Result == "" || ends[id].NonTransient {
			t.Fatalf("lost correlation or inferred retryability: %s", id)
		}
	}
	if ends["a"].Outcome != agent.ToolSucceeded || ends["b"].Outcome != agent.ToolFailed {
		t.Fatal("lost result outcomes")
	}
}
