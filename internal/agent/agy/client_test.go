package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestClientRunParsesStreamSessionAndArguments(t *testing.T) {
	binary, argsPath := fakeAGY(t)
	workspace := t.TempDir()
	extra := t.TempDir()
	client := New(Config{
		Binary: binary, SessionID: "previous-conversation", Model: "gemini-test", Effort: "high", Permissions: chat.PermissionWorkspace,
	})
	var events []agent.Event
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "hello room", Workspace: workspace, ReadRoots: []string{workspace, extra}, WriteRoots: []string{workspace}, SystemPrompt: "system rules",
		Settings: chat.AgentSettings{Model: "gemini-test", Effort: "high", Permissions: chat.PermissionWorkspace},
	}, func(event agent.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello from AGY" || !result.Done || result.SessionID != "agy-conversation" {
		t.Fatalf("result=%+v", result)
	}
	if client.SessionID() != "agy-conversation" {
		t.Fatalf("client session=%q", client.SessionID())
	}
	args := readArgs(t, argsPath)
	for _, pair := range [][2]string{
		{"--input-format", "stream-json"},
		{"--output-format", "stream-json"},
		{"--conversation", "previous-conversation"},
		{"--model", "gemini-test"},
		{"--effort", "high"},
		{"--mode", "accept-edits"},
		{"--add-dir", extra},
	} {
		if !hasArgPair(args, pair[0], pair[1]) {
			t.Errorf("missing arguments %q %q in %v", pair[0], pair[1], args)
		}
	}
	if !hasArg(args, "--sandbox") || !hasArg(args, "--dangerously-skip-permissions") {
		t.Fatalf("workspace profile arguments=%v", args)
	}
	inputData, err := os.ReadFile(argsPath + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(inputData, &input); err != nil {
		t.Fatal(err)
	}
	if input.Event != "user" || !strings.Contains(input.Message.Content, "system rules") || !strings.Contains(input.Message.Content, "hello room") {
		t.Fatalf("input=%+v", input)
	}
	seenDelta, seenTool := false, false
	for _, event := range events {
		seenDelta = seenDelta || (event.Type == agent.EventDelta && event.Text == "hello from AGY")
		seenTool = seenTool || (event.Type == agent.EventTool && strings.Contains(event.Text, "go test ./..."))
	}
	if !seenDelta || !seenTool {
		t.Fatalf("events=%+v", events)
	}
}

func TestPermissionProfilesMapToAGYFlags(t *testing.T) {
	tests := []struct {
		profile                     chat.PermissionProfile
		mode                        string
		wantSandbox, wantSkipReview bool
	}{
		{chat.PermissionReadOnly, "plan", true, true},
		{chat.PermissionWorkspace, "accept-edits", true, true},
		{chat.PermissionFull, "accept-edits", false, true},
	}
	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			binary, argsPath := fakeAGY(t)
			workspace := t.TempDir()
			client := New(Config{Binary: binary})
			_, err := client.Run(context.Background(), agent.TurnRequest{
				Prompt: "test", Workspace: workspace, ReadRoots: []string{workspace}, WriteRoots: []string{workspace}, SystemPrompt: "system",
				Settings: chat.AgentSettings{Permissions: test.profile},
			}, func(agent.Event) {})
			if err != nil {
				t.Fatal(err)
			}
			args := readArgs(t, argsPath)
			if !hasArgPair(args, "--mode", test.mode) {
				t.Fatalf("arguments=%v", args)
			}
			if got := hasArg(args, "--sandbox"); got != test.wantSandbox {
				t.Errorf("sandbox=%v want=%v args=%v", got, test.wantSandbox, args)
			}
			if got := hasArg(args, "--dangerously-skip-permissions"); got != test.wantSkipReview {
				t.Errorf("skip permissions=%v want=%v args=%v", got, test.wantSkipReview, args)
			}
			if got := hasArg(args, "--disable-slash-commands"); got != (test.profile != chat.PermissionReadOnly) {
				t.Errorf("disable slash commands=%v profile=%s args=%v", got, test.profile, args)
			}
		})
	}
}

func TestVoiceOnlyUsesIsolatedDirectSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-agy-voice")
	argsPath := filepath.Join(dir, "voice-args.txt")
	t.Setenv("MOHUDDLE_AGY_ARGS", argsPath)
	script := `#!/bin/sh
: > "$MOHUDDLE_AGY_ARGS"
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$MOHUDDLE_AGY_ARGS"
done
pwd > "$MOHUDDLE_AGY_ARGS.cwd"
cat >/dev/null
printf '%s\n' '{"event":"result","result":{"conversation_id":"voice-conversation","status":"SUCCESS","response":"voice answer\n<!-- mohuddle:{\"done\":true} -->"}}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	client := New(Config{Binary: binary, SessionID: "saved-conversation", Permissions: chat.PermissionWorkspace})
	result, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "speak", Workspace: workspace, ReadRoots: []string{workspace}, WriteRoots: []string{workspace},
		SystemPrompt: "voice system", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly}, VoiceOnly: true,
	}, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "voice answer" || result.SessionID != "" || client.SessionID() != "saved-conversation" {
		t.Fatalf("result=%+v saved=%q", result, client.SessionID())
	}
	args := readArgs(t, argsPath)
	if !hasArg(args, "--disable-slash-commands") || !hasArg(args, "--sandbox") || hasArg(args, "--agent") || hasArg(args, "--mode") || hasArg(args, "--dangerously-skip-permissions") || hasArg(args, "--conversation") || hasArg(args, "--add-dir") {
		t.Fatalf("voice arguments=%v", args)
	}
	cwd, err := os.ReadFile(argsPath + ".cwd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(cwd)) == workspace {
		t.Fatalf("voice turn ran in project workspace %q", workspace)
	}
}

func TestVoiceOnlyFailsClosedOnUnexpectedToolEvent(t *testing.T) {
	binary, _ := fakeAGY(t)
	client := New(Config{Binary: binary})
	_, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "speak", Workspace: t.TempDir(), SystemPrompt: "voice", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly}, VoiceOnly: true,
	}, func(agent.Event) {})
	if err == nil || !strings.Contains(err.Error(), "isolated read-only turn attempted to use a tool") {
		t.Fatalf("unexpected voice tool result: %v", err)
	}
}

func TestVoiceOnlyRetriesMarkerOnlyResultWithFocusedPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-agy-empty-then-answer")
	countPath := filepath.Join(dir, "calls")
	t.Setenv("MOHUDDLE_AGY_CALLS", countPath)
	script := `#!/bin/sh
count=0
if [ -f "$MOHUDDLE_AGY_CALLS" ]; then
  count=$(cat "$MOHUDDLE_AGY_CALLS")
fi
count=$((count + 1))
printf '%s' "$count" > "$MOHUDDLE_AGY_CALLS"
cat > "$MOHUDDLE_AGY_CALLS.stdin.$count"
if [ "$count" -eq 1 ]; then
  printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"<!-- mohuddle:{\"done\":true,\"position\":\"neutral\",\"reason\":\"\",\"next\":\"\"} -->"}}'
else
  printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"Hello from AGY.\n<!-- mohuddle:{\"done\":true,\"position\":\"neutral\",\"reason\":\"\",\"next\":\"\"} -->"}}'
fi
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	longHistory := strings.Repeat("older transcript detail ", voicePromptRuneLimit*2)
	request := agent.TurnRequest{
		Prompt:    longHistory + "\n[999] user -> agy (message):\nintroduce yourself\n\nEND UNTRUSTED ROOM TRANSCRIPT\n\nRespond to the room now.",
		Workspace: t.TempDir(), SystemPrompt: "voice system", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly},
		VoiceOnly: true, PublicResponseRequired: true,
	}
	result, err := New(Config{Binary: binary}).Run(context.Background(), request, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Hello from AGY." {
		t.Fatalf("result=%+v", result)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "2" {
		t.Fatalf("calls=%q", count)
	}
	secondInput, err := os.ReadFile(countPath + ".stdin.2")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(secondInput, []byte("introduce yourself")) || !bytes.Contains(secondInput, []byte("previous attempt returned only")) {
		t.Fatalf("focused retry did not retain latest request: %s", secondInput)
	}
	if len([]rune(string(secondInput))) >= voicePromptRuneLimit {
		t.Fatalf("focused retry remained too large: %d runes", len([]rune(string(secondInput))))
	}
}

func TestVoiceOnlyFailsAfterSecondMarkerOnlyResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-agy-always-empty")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"<!-- mohuddle:{\"done\":true} -->"}}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{Binary: binary}).Run(context.Background(), agent.TurnRequest{
		Prompt: "speak", Workspace: dir, SystemPrompt: "voice", Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly}, VoiceOnly: true,
		PublicResponseRequired: true,
	}, func(agent.Event) {})
	if err == nil || !strings.Contains(err.Error(), "returned no public response") {
		t.Fatalf("unexpected empty voice result: %v", err)
	}
}

func TestVoiceOnlyAcceptsMarkerOnlyOptionalTurnWithoutRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-agy-optional-silence")
	countPath := filepath.Join(dir, "calls")
	t.Setenv("MOHUDDLE_AGY_CALLS", countPath)
	script := `#!/bin/sh
printf 'called\n' >> "$MOHUDDLE_AGY_CALLS"
cat >/dev/null
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"<!-- mohuddle:{\"done\":true} -->"}}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := New(Config{Binary: binary}).Run(context.Background(), agent.TurnRequest{
		Prompt: "review only if useful", Workspace: dir, SystemPrompt: "voice",
		Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly}, VoiceOnly: true,
	}, func(agent.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "" || !result.Done {
		t.Fatalf("result=%+v", result)
	}
	calls, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "called") != 1 {
		t.Fatalf("optional silence was retried: %q", calls)
	}
}

func TestModelsParsesAGYCatalog(t *testing.T) {
	binary, _ := fakeAGY(t)
	models, err := New(Config{Binary: binary}).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gemini-test-high" || models[0].Name != "Gemini Test (High)" || models[1].ID != "claude-test" {
		t.Fatalf("models=%+v", models)
	}
}

func TestCanceledResultWrapsContextCanceled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-canceled-agy")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"event":"result","result":{"status":"CANCELED","error":"context canceled"}}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := New(Config{Binary: binary})
	_, err := client.Run(context.Background(), agent.TurnRequest{
		Prompt: "test", Workspace: dir, SystemPrompt: "system",
		Settings: chat.AgentSettings{Permissions: chat.PermissionReadOnly},
	}, func(agent.Event) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled AGY result did not wrap context.Canceled: %v", err)
	}
}

func fakeAGY(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-agy")
	argsPath := filepath.Join(dir, "args.txt")
	t.Setenv("MOHUDDLE_AGY_ARGS", argsPath)
	script := `#!/bin/sh
if [ "$1" = "models" ]; then
  printf '%s\n' 'Available models:'
  printf '%s\n' 'gemini-test-high  Gemini Test (High)'
  printf '%s\n' 'claude-test       Claude Test'
  exit 0
fi
: > "$MOHUDDLE_AGY_ARGS"
for arg in "$@"; do
  printf '%s\n' "$arg" >> "$MOHUDDLE_AGY_ARGS"
done
cat > "$MOHUDDLE_AGY_ARGS.stdin"
printf '%s\n' '{"event":"init","conversation_id":"agy-conversation"}'
printf '%s\n' '{"event":"step_update","step_update":{"conversation_id":"agy-conversation","state":"ACTIVE","step_type":"agent_response","text_delta":"hello from AGY"}}'
printf '%s\n' '{"event":"step_update","step_update":{"conversation_id":"agy-conversation","state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"parameters":{"CommandLine":"go test ./..."}}}}'
printf '%s\n' '{"event":"result","result":{"conversation_id":"agy-conversation","status":"SUCCESS","response":"hello from AGY\n<!-- mohuddle:{\"done\":true} -->"}}'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, argsPath
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func hasArg(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

func hasArgPair(arguments []string, flag, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == value {
			return true
		}
	}
	return false
}
