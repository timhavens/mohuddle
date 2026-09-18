package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestHostReadOnlyPolicyConstrainsProviderRequest(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	policy := chat.ResolveTurnAccess(chat.PermissionFull, true, false)
	request := EnforceTurnAccess(TurnRequest{Access: policy, Settings: chat.AgentSettings{Permissions: chat.PermissionFull}, Workspace: workspace, WriteRoots: []string{workspace}})
	if request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 {
		t.Fatal("write capability survived read-only ceiling")
	}
	covered := false
	for _, root := range request.ReadRoots {
		covered = covered || access.Contains(root, filepath.Join(outside, "diagnostic.txt"))
	}
	if !covered {
		t.Fatalf("outside diagnostic not readable: %v", request.ReadRoots)
	}
	prompt := RoomProtocolPromptFor(chat.Codex, request.Settings, request.Access)
	if strings.Contains(prompt, "If you need a directory outside") || !strings.Contains(prompt, "full-machine grant remains valid") {
		t.Fatal("prompt contradicts filesystem authorization")
	}
	request.NoTools = true
	request = EnforceTurnAccess(request)
	if len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
		t.Fatal("tool-free turn retained filesystem roots")
	}
}
