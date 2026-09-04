package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestSettingsEnforceReadOnlyRoots(t *testing.T) {
	request := agent.TurnRequest{
		ReadRoots:  []string{"/workspace", "/context"},
		WriteRoots: []string{"/workspace"},
	}
	data, err := settingsJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	permissions := settings["permissions"].(map[string]any)
	deny := permissions["deny"].([]any)
	if len(deny) != 1 || deny[0] != "Edit(//context/**)" {
		t.Fatalf("unexpected edit deny rules: %v", deny)
	}
	sandbox := settings["sandbox"].(map[string]any)
	filesystem := sandbox["filesystem"].(map[string]any)
	denyWrite := filesystem["denyWrite"].([]any)
	if len(denyWrite) != 1 || denyWrite[0] != "/context" {
		t.Fatalf("unexpected denyWrite: %v", denyWrite)
	}
}

func TestSettingsPermissionProfiles(t *testing.T) {
	request := agent.TurnRequest{ReadRoots: []string{"/workspace"}, WriteRoots: []string{"/workspace"}}
	readOnlyData, err := settingsJSON(request, chat.PermissionReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	var readOnly map[string]any
	if err := json.Unmarshal(readOnlyData, &readOnly); err != nil {
		t.Fatal(err)
	}
	permissions := readOnly["permissions"].(map[string]any)
	if permissions["defaultMode"] != "plan" {
		t.Fatalf("read-only defaultMode=%v", permissions["defaultMode"])
	}
	filesystem := readOnly["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if roots := filesystem["allowWrite"].([]any); len(roots) != 0 {
		t.Fatalf("read-only allowWrite=%v", roots)
	}
	network := readOnly["sandbox"].(map[string]any)["network"].(map[string]any)
	if domains := network["allowedDomains"].([]any); len(domains) != 0 {
		t.Fatalf("read-only allowedDomains=%v", domains)
	}

	workspaceData, err := settingsJSON(request, chat.PermissionWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	var workspace map[string]any
	if err := json.Unmarshal(workspaceData, &workspace); err != nil {
		t.Fatal(err)
	}
	workspaceNetwork := workspace["sandbox"].(map[string]any)["network"].(map[string]any)
	if domains := workspaceNetwork["allowedDomains"].([]any); len(domains) != 0 {
		t.Fatalf("workspace allowedDomains=%v", domains)
	}

	fullData, err := settingsJSON(request, chat.PermissionFull)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]any
	if err := json.Unmarshal(fullData, &full); err != nil {
		t.Fatal(err)
	}
	if enabled := full["sandbox"].(map[string]any)["enabled"]; enabled != false {
		t.Fatalf("full sandbox enabled=%v", enabled)
	}
	if mode := full["permissions"].(map[string]any)["defaultMode"]; mode != "bypassPermissions" {
		t.Fatalf("full defaultMode=%v", mode)
	}
}

func TestParseAssistant(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}`)
	text, tools := parseAssistant(raw)
	if text != "working" || len(tools) != 1 || tools[0] != "Bash: go test ./..." {
		t.Fatalf("unexpected parse: text=%q tools=%v", text, tools)
	}
}

func TestClientRunParsesStreamAndSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session","model":"claude-runtime","reasoning_effort":"high"}'
printf '%s\n' '{"type":"assistant","session_id":"claude-session","message":{"content":[{"type":"text","text":"hello from claude"},{"type":"tool_use","name":"Read","input":{"file_path":"README.md"}}]}}'
printf '%s\n' '{"type":"result","subtype":"success","session_id":"claude-session","result":"hello from claude\n<!-- mohuddle:{\"done\":true} -->","is_error":false}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := New(Config{Binary: binary})
	var events []agent.Event
	result, err := client.Run(context.Background(), agent.TurnRequest{Prompt: "hello", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, SystemPrompt: "system"}, func(event agent.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello from claude" || !result.Done || result.SessionID != "claude-session" || result.RuntimeModel != "claude-runtime" || result.RuntimeEffort != "high" || result.RuntimeSource != "claude system/init" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(events) < 3 {
		t.Fatalf("expected streamed events, got %+v", events)
	}
}

func TestClientErrorPreservesSessionLimitFromResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-claude-limit")
	reaped := filepath.Join(dir, "reaped")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"error","session_id":"limited-session","result":"You have hit your session limit · resets 1:20am (America/Port-au-Prince)","is_error":true,"errors":[]}'
sleep 0.1
printf done > "$MOHUDDLE_REAP_MARKER"
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := New(Config{Binary: binary})
	t.Setenv("MOHUDDLE_REAP_MARKER", reaped)
	_, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "hello", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir}, SystemPrompt: "system",
	}, func(agent.Event) {})
	if err == nil || !strings.Contains(err.Error(), "session limit") || !strings.Contains(err.Error(), "1:20am") {
		t.Fatalf("error=%v", err)
	}
	if data, readErr := os.ReadFile(reaped); readErr != nil || string(data) != "done" {
		t.Fatalf("Claude child was not reaped before Run returned: data=%q err=%v", data, readErr)
	}
}

func TestClientNoToolsTurnUsesNativeEmptyAllowlistAndIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-claude-no-tools")
	script := `#!/bin/sh
if [ "$PWD" = "$MOHUDDLE_ORIGINAL_WORKSPACE" ]; then
  exit 7
fi
found=false
previous=
for argument in "$@"; do
  if [ "$previous" = "--tools" ] && [ -z "$argument" ]; then
    found=true
  fi
  if [ "$argument" = "$MOHUDDLE_ORIGINAL_WORKSPACE" ]; then
    exit 8
  fi
  previous="$argument"
done
if [ "$found" != "true" ]; then
  exit 9
fi
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success","session_id":"private-session","result":"bid\n<!-- mohuddle:{\"done\":true} -->","is_error":false}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_ORIGINAL_WORKSPACE", dir)
	client := New(Config{Binary: binary})
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "route", Workspace: dir, ReadRoots: []string{dir}, WriteRoots: []string{dir},
		SystemPrompt: "system", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly},
		Ephemeral: true, NoTools: true,
	}, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) != "bid" || result.SessionID != "" {
		t.Fatalf("unexpected private result: %+v", result)
	}
}
