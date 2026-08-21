package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session"}'
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
	if result.Text != "hello from claude" || !result.Done || result.SessionID != "claude-session" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(events) < 3 {
		t.Fatalf("expected streamed events, got %+v", events)
	}
}
