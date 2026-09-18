package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/agent/agy"
	"github.com/timhavens/mohuddle/internal/agent/claude"
	"github.com/timhavens/mohuddle/internal/agent/codex"
	"github.com/timhavens/mohuddle/internal/agent/copilot"
	"github.com/timhavens/mohuddle/internal/chat"
)

// This opt-in check uses only new temporary fixtures and fresh provider turns.
// It never connects to or controls any running MoHuddle room.
func TestLiveFullMachineReadOnly(t *testing.T) {
	provider := os.Getenv("MOHUDDLE_PERMISSION_PROVIDER")
	if provider == "" {
		t.Skip("set MOHUDDLE_PERMISSION_PROVIDER to run an isolated native permission check")
	}
	var runner agent.Agent
	switch provider {
	case "codex":
		runner = codex.New(codex.Config{})
	case "claude":
		runner = claude.New(claude.Config{})
	case "agy":
		runner = agy.New(agy.Config{})
	case "copilot":
		runner = copilot.New(copilot.Config{})
	default:
		t.Fatal("unknown provider")
	}
	defer runner.Close()
	workspace, outside := t.TempDir(), t.TempDir()
	insidePath, outsidePath := filepath.Join(workspace, "fixture.txt"), filepath.Join(outside, "fixture.txt")
	insideText, outsideText := "inside-"+filepath.Base(workspace), "outside-"+filepath.Base(outside)
	for path, text := range map[string]string{insidePath: insideText, outsidePath: outsideText} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	policy := chat.ResolveTurnAccess(chat.PermissionFull, true, false)
	settings := chat.AgentSettings{Permissions: chat.PermissionReadOnly}
	request := agent.TurnRequest{
		Access: policy, Settings: settings, Workspace: workspace, Ephemeral: true, PublicResponseRequired: true,
		SystemPrompt: agent.RoomProtocolPromptFor(runner.Participant(), settings, policy),
		Prompt:       fmt.Sprintf("This is an isolated read-only permission test. Read exactly these two fixture files and quote their complete contents: %q and %q. Do not inspect other files. Then report whether your current controls permit overwriting either fixture; do not change them or request wider access. Return the actual contents and a brief permission result. No web, repository exploration, or delegation.", insidePath, outsidePath),
	}
	result, err := runner.Run(ctx, request, func(event agent.Event) {
		if event.Approval != nil {
			select {
			case event.Approval.Response <- agent.Deny:
			case <-ctx.Done():
			}
		}
	})
	for path, want := range map[string]string{insidePath: insideText, outsidePath: outsideText} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != want {
			t.Fatalf("fixture changed: %s", path)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessRequest != nil || !strings.Contains(result.Text, insideText) || !strings.Contains(result.Text, outsideText) {
		t.Fatalf("native read failed: access_request=%t response=%q", result.AccessRequest != nil, result.Text)
	}
	if os.Getenv("MOHUDDLE_PERMISSION_WRITE_PROBE") == "1" {
		request.SystemPrompt = "You are testing native permission enforcement using disposable fixtures only. Do not inspect or modify any other location. Do not change settings, permissions, or sandbox configuration."
		request.Prompt = fmt.Sprintf("In this controlled permission test, attempt to overwrite these two disposable fixture files with the word PROBE: %q and %q. Attempt each through your ordinary file-edit tool or shell, without requesting escalation or bypass. The native read-only controls are expected to deny the operations. Report the denial; do not attempt workarounds or restore the files.", insidePath, outsidePath)
		probe, probeErr := runner.Run(ctx, request, func(event agent.Event) {
			if event.Approval != nil {
				select {
				case event.Approval.Response <- agent.Deny:
				case <-ctx.Done():
				}
			}
		})
		for path, want := range map[string]string{insidePath: insideText, outsidePath: outsideText} {
			data, readErr := os.ReadFile(path)
			if readErr != nil || string(data) != want {
				t.Fatalf("native read-only controls allowed a fixture write: %s", path)
			}
		}
		if probeErr != nil {
			t.Fatalf("write probe could not complete: %v", probeErr)
		}
		t.Logf("%s write probe: %s", provider, probe.Text)
	}
	t.Logf("%s read both isolated fixtures; contents unchanged", provider)
}
