package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestLiveCodexHostInstructionsSurviveResumeAndAccessChanges(t *testing.T) {
	if os.Getenv("MOHUDDLE_LIVE") != "1" {
		t.Skip("set MOHUDDLE_LIVE=1 to use the authenticated Codex CLI")
	}
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run := func(client *Client, permissions chat.PermissionProfile, prompt string) agent.TurnResult {
		t.Helper()
		settings := chat.AgentSettings{Model: "gpt-6-astra", Effort: "low", Permissions: permissions}
		result, err := client.Run(ctx, agent.TurnRequest{
			Workspace: workspace, ReadRoots: []string{workspace}, WriteRoots: []string{workspace},
			Settings: settings, Prompt: prompt,
			SystemPrompt: agent.RoomProtocolPromptFor(chat.Codex, settings) + "\nThis is an isolated integration test. Work only in this temporary workspace. No project or MCP lookup is needed.",
		}, func(event agent.Event) {
			if event.Approval != nil {
				event.Approval.Response <- agent.Deny
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("permissions=%s session=%s response=%s", permissions, result.SessionID, result.Text)
		return result
	}
	first := New(Config{})
	defer first.Close()
	initial := run(first, chat.PermissionReadOnly, "Remember the test marker: mohuddle-policy-refresh. Do not use tools. Reply READY.")
	if initial.SessionID == "" {
		t.Fatal("missing initial session")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := New(Config{SessionID: initial.SessionID})
	defer resumed.Close()
	written := run(resumed, chat.PermissionWorkspace, "Create policy-smoke.txt containing only the test marker I gave you earlier and a newline. Verify the file. This is the authorized editing step of the integration test.")
	if written.SessionID != initial.SessionID {
		t.Fatal("native conversation history was discarded")
	}
	content, err := os.ReadFile(filepath.Join(workspace, "policy-smoke.txt"))
	if err != nil || string(content) != "mohuddle-policy-refresh\n" {
		t.Fatalf("resumed edit failed: %q, %v", content, err)
	}
	run(resumed, chat.PermissionReadOnly, "Create forbidden.txt now. If the current host access restrictions prohibit that edit, refuse it briefly instead.")
	if _, err := os.Stat(filepath.Join(workspace, "forbidden.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only step modified the workspace: %v", err)
	}
	run(resumed, chat.PermissionWorkspace, "Create policy-second.txt containing exactly: writable again\n. Verify the file. This is another authorized editing step.")
	content, err = os.ReadFile(filepath.Join(workspace, "policy-second.txt"))
	if err != nil || string(content) != "writable again\n" {
		t.Fatalf("second edit failed: %q, %v", content, err)
	}
}
