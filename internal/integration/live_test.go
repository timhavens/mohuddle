package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/agent/claude"
	"github.com/timhavens/mohuddle/internal/agent/codex"
)

func TestLiveCodingAgentsShareWorkspace(t *testing.T) {
	if os.Getenv("MOHUDDLE_LIVE") != "1" {
		t.Skip("set MOHUDDLE_LIVE=1 to use the authenticated Codex and Claude CLIs")
	}
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	claudeAgent := claude.New(claude.Config{})
	defer claudeAgent.Close()
	claudeResult, err := claudeAgent.Run(ctx, agent.TurnRequest{
		Prompt:       "Create shared-agent-smoke.txt in the workspace with exactly one line: claude was here. Do it now, verify the file, and report briefly.",
		Workspace:    workspace,
		ReadRoots:    []string{workspace},
		WriteRoots:   []string{workspace},
		SystemPrompt: agent.RoomProtocolPrompt,
	}, approveAll)
	if err != nil {
		t.Fatal(err)
	}
	if claudeResult.SessionID == "" {
		t.Fatal("Claude did not return a session ID")
	}

	codexAgent := codex.New(codex.Config{})
	defer codexAgent.Close()
	codexResult, err := codexAgent.Run(ctx, agent.TurnRequest{
		Prompt:       "Read shared-agent-smoke.txt, then append exactly one new line: codex was here. Verify the final file and report briefly.",
		Workspace:    workspace,
		ReadRoots:    []string{workspace},
		WriteRoots:   []string{workspace},
		SystemPrompt: agent.RoomProtocolPrompt,
	}, approveAll)
	if err != nil {
		t.Fatal(err)
	}
	if codexResult.SessionID == "" {
		t.Fatal("Codex did not return a session ID")
	}
	data, err := os.ReadFile(filepath.Join(workspace, "shared-agent-smoke.txt"))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.Contains(content, "claude was here") || !strings.Contains(content, "codex was here") {
		t.Fatalf("unexpected shared file content: %q", content)
	}

	resumedClaude := claude.New(claude.Config{SessionID: claudeResult.SessionID})
	defer resumedClaude.Close()
	resumeResult, err := resumedClaude.Run(ctx, agent.TurnRequest{
		Prompt:       "Read shared-agent-smoke.txt and report its two lines without changing it.",
		Workspace:    workspace,
		ReadRoots:    []string{workspace},
		WriteRoots:   []string{workspace},
		SystemPrompt: agent.RoomProtocolPrompt,
	}, approveAll)
	if err != nil {
		t.Fatal(err)
	}
	if resumeResult.SessionID != claudeResult.SessionID {
		t.Fatalf("Claude resumed with a different session: got %q want %q", resumeResult.SessionID, claudeResult.SessionID)
	}
}

func approveAll(event agent.Event) {
	if event.Approval != nil {
		event.Approval.Response <- agent.ApproveOnce
	}
}
