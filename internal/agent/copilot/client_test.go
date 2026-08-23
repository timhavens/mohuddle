package copilot

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestCopilotToolsRespectPermissionProfile(t *testing.T) {
	voice := copilotTools(chat.PermissionReadOnly, true)
	if voice == nil || len(voice) != 0 {
		t.Fatalf("voice-only tools=%v; want explicit empty allowlist", voice)
	}
	readOnly := copilotTools(chat.PermissionReadOnly)
	if !slices.Contains(readOnly, "builtin:view") || !slices.Contains(readOnly, "builtin:grep") || slices.Contains(readOnly, "builtin:edit") || slices.Contains(readOnly, "builtin:bash") {
		t.Fatalf("read-only tools=%v", readOnly)
	}
	workspace := copilotTools(chat.PermissionWorkspace)
	for _, tool := range []string{"builtin:view", "builtin:grep", "builtin:edit", "builtin:bash", "builtin:powershell"} {
		if !slices.Contains(workspace, tool) {
			t.Errorf("workspace tools missing %q: %v", tool, workspace)
		}
	}
	if slices.Contains(workspace, "builtin:ask_user") {
		t.Fatalf("workspace tools expose ask_user without a TUI handler: %v", workspace)
	}
	full := copilotTools(chat.PermissionFull)
	if !slices.Contains(full, "builtin:*") {
		t.Fatalf("full tools=%v", full)
	}
}

func TestPermissionDecisionScopesReadsWritesAndShell(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	contextRoot := filepath.Join(t.TempDir(), "context")
	client := &Client{policy: accessPolicy{
		profile: chat.PermissionWorkspace, workspace: workspace,
		readRoots: []string{workspace, contextRoot}, writeRoots: []string{workspace},
	}}

	assertApproved(t, client, rpc.PermissionRequestRead{Path: filepath.Join(contextRoot, "notes.txt")})
	assertRejected(t, client, rpc.PermissionRequestRead{Path: filepath.Join(filepath.Dir(contextRoot), "outside.txt")})
	assertApproved(t, client, rpc.PermissionRequestWrite{FileName: filepath.Join(workspace, "main.go")})
	assertRejected(t, client, rpc.PermissionRequestWrite{FileName: filepath.Join(contextRoot, "notes.txt")})
	assertApproved(t, client, rpc.PermissionRequestShell{FullCommandText: "go test ./...", PossiblePaths: []string{workspace}})
	assertRejected(t, client, rpc.PermissionRequestShell{
		FullCommandText: "rm notes.txt", PossiblePaths: []string{filepath.Join(contextRoot, "notes.txt")},
		Commands: []rpc.PermissionRequestShellCommand{{Identifier: "rm", ReadOnly: false}},
	})
	assertRejected(t, client, rpc.PermissionRequestShell{FullCommandText: "curl https://example.com"})
	assertRejected(t, client, rpc.PermissionRequestShell{
		FullCommandText: "/usr/bin/curl", Commands: []rpc.PermissionRequestShellCommand{{Identifier: "/usr/bin/curl", ReadOnly: false}},
	})
	assertRejected(t, client, rpc.PermissionRequestShell{FullCommandText: "tool", PossibleURLs: []rpc.PermissionRequestShellPossibleURL{{URL: "https://example.com"}}})
	bypass := true
	assertRejected(t, client, rpc.PermissionRequestShell{FullCommandText: "go test ./...", RequestSandboxBypass: &bypass})
}

func TestReadOnlyAndManagedPoliciesRejectMutation(t *testing.T) {
	workspace := t.TempDir()
	client := &Client{policy: accessPolicy{
		profile: chat.PermissionReadOnly, workspace: workspace,
		readRoots: []string{workspace}, writeRoots: []string{workspace},
	}}
	assertApproved(t, client, rpc.PermissionRequestRead{Path: filepath.Join(workspace, "README.md")})
	assertRejected(t, client, rpc.PermissionRequestWrite{FileName: filepath.Join(workspace, "README.md")})
	assertRejected(t, client, rpc.PermissionRequestShell{FullCommandText: "pwd"})

	decision, err := client.permissionDecision(rpc.PermissionRequestRead{Path: filepath.Join(workspace, "README.md")}, sdk.PermissionInvocation{ManagedSettingsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decision.(*rpc.PermissionDecisionReject); !ok {
		t.Fatalf("managed-policy decision=%T", decision)
	}
}

func TestFullProfileApprovesProviderRequests(t *testing.T) {
	client := &Client{policy: accessPolicy{profile: chat.PermissionFull}}
	assertApproved(t, client, rpc.PermissionRequestURL{URL: "https://example.com"})
	assertApproved(t, client, rpc.PermissionRequestWrite{FileName: "/outside/workspace"})
}

func TestAdditionalDirectories(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	request := agent.TurnRequest{Workspace: workspace, ReadRoots: []string{workspace, extra, extra}}
	if got := additionalDirectories(chat.PermissionWorkspace, request); len(got) != 1 || got[0] != extra {
		t.Fatalf("workspace directories=%v", got)
	}
	full := additionalDirectories(chat.PermissionFull, request)
	wantRoot := filepath.VolumeName(workspace) + string(filepath.Separator)
	if len(full) != 1 || full[0] != wantRoot {
		t.Fatalf("full directories=%v want=%q", full, wantRoot)
	}
}

func TestImageAttachmentsBecomeCopilotFileAttachments(t *testing.T) {
	attachments := copilotAttachments([]chat.Attachment{
		{Kind: chat.AttachmentImage, Name: "screen.png", Path: "/tmp/screen.png"},
		{Kind: chat.AttachmentImage, Name: "missing.png"},
	})
	if len(attachments) != 1 {
		t.Fatalf("attachments=%v", attachments)
	}
	file, ok := attachments[0].(sdk.AttachmentFile)
	if !ok || file.DisplayName != "screen.png" || file.Path != "/tmp/screen.png" {
		t.Fatalf("attachment=%T %+v", attachments[0], attachments[0])
	}
}

func TestCopilotQuotaErrorsAreTypedAvailabilitySignals(t *testing.T) {
	code := "session_quota_exceeded"
	turnErr := copilotSessionError(&sdk.SessionErrorData{ErrorType: "quota", ErrorCode: &code, Message: "quota exhausted"})
	var availability *agent.AvailabilityError
	if !errors.As(turnErr, &availability) || availability.Participant != chat.Copilot || availability.Source != "copilot-sdk" || availability.Confidence != "confirmed" {
		t.Fatalf("availability error=%T %+v", turnErr, availability)
	}
	ordinary := copilotSessionError(&sdk.SessionErrorData{ErrorType: "query", Message: "bad query"})
	if errors.As(ordinary, &availability) {
		t.Fatalf("ordinary error was classified as availability: %v", ordinary)
	}
	for _, scoped := range []string{"user_model_rate_limited", "integration_rate_limited", "billing_not_configured"} {
		scoped := scoped
		errorType := "rate_limit"
		if scoped == "billing_not_configured" {
			errorType = "quota"
		}
		ordinary = copilotSessionError(&sdk.SessionErrorData{ErrorType: errorType, ErrorCode: &scoped, Message: "scoped failure"})
		if errors.As(ordinary, &availability) {
			t.Fatalf("%s was classified as participant availability: %v", scoped, ordinary)
		}
	}
}

func assertApproved(t *testing.T, client *Client, request sdk.PermissionRequest) {
	t.Helper()
	decision, err := client.permissionDecision(request, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decision.(*rpc.PermissionDecisionApproveOnce); !ok {
		t.Fatalf("decision for %T=%T, want approval", request, decision)
	}
}

func assertRejected(t *testing.T, client *Client, request sdk.PermissionRequest) {
	t.Helper()
	decision, err := client.permissionDecision(request, sdk.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decision.(*rpc.PermissionDecisionReject); !ok {
		t.Fatalf("decision for %T=%T, want rejection", request, decision)
	}
}
