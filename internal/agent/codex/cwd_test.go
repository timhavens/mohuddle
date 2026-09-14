package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func codexCWDTestBinary(t *testing.T, helper string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test helper requires POSIX shell and removable process directories")
	}
	binary := filepath.Join(t.TempDir(), "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^%s$ -- \"$@\"\n", os.Args[0], helper)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

func removeTestCWD(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if testCWDIsReachable() {
		t.Fatal("test did not invalidate the inherited directory")
	}
}

// Darwin can return a removed directory's old name from Getwd. Check whether
// that name still resolves to the process's actual directory as well.
func testCWDIsReachable() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	actual, actualErr := os.Stat(".")
	named, namedErr := os.Stat(cwd)
	return actualErr == nil && namedErr == nil && os.SameFile(actual, named)
}

func TestClientStartsFromDeletedParentDirectory(t *testing.T) {
	binary := codexCWDTestBinary(t, "TestCodexHelperProcess")
	workspace := t.TempDir()
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	t.Setenv("MOHUDDLE_EXPECTED_PROCESS_CWD", workspace)
	removeTestCWD(t)
	client := New(Config{Binary: binary})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Run(ctx, agent.TurnRequest{
		Workspace: workspace, Prompt: "hello",
		Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace},
	}, func(event agent.Event) {
		if event.Approval != nil {
			event.Approval.Response <- agent.ApproveOnce
		}
	})
	if err != nil || result.Text != "hello from codex" {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
}

func TestClientModelsFromDeletedParentDirectory(t *testing.T) {
	binary := codexCWDTestBinary(t, "TestCodexHelperProcess")
	for _, useWorkspace := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%v", useWorkspace), func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("HOME", workspace)
			t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
			t.Setenv("MOHUDDLE_EXPECTED_PROCESS_CWD", workspace)
			removeTestCWD(t)
			client := New(Config{Binary: binary})
			defer client.Close()
			if useWorkspace {
				client.workspace = workspace
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			models, err := client.Models(ctx)
			if err != nil || len(models) != 1 || models[0].ID != "gpt-test" {
				t.Fatalf("Models() = %+v, %v", models, err)
			}
		})
	}
}

func TestClientResolvesRelativeBinaryBeforeEnteringWorkspace(t *testing.T) {
	binary := codexCWDTestBinary(t, "TestCodexHelperProcess")
	workspace := t.TempDir()
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	t.Setenv("MOHUDDLE_EXPECTED_PROCESS_CWD", workspace)
	t.Chdir(filepath.Dir(binary))
	client := New(Config{Binary: "./fake-codex", Permissions: chat.PermissionReadOnly})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.ensureStarted(ctx, agent.TurnRequest{
		Workspace: workspace, NoTools: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClientRecoversStaleCWDWithoutRepeatingAcceptedTurns(t *testing.T) {
	binary := codexCWDTestBinary(t, "TestCodexCWDHelperProcess")
	for _, test := range []struct {
		mode       string
		wantError  string
		starts     int
		accepted   int
		rejections int
	}{
		{mode: "recover", starts: 2, accepted: 2, rejections: 1},
		{mode: "repeat", wantError: "still unavailable after restarting", starts: 2, accepted: 1, rejections: 2},
		{mode: "missing", wantError: "workspace", starts: 1, accepted: 1, rejections: 1},
		{mode: "unrelated", wantError: "invalid model", starts: 1, accepted: 1, rejections: 1},
		{mode: "post-start", wantError: "codex turn failed", starts: 1, accepted: 2},
	} {
		t.Run(test.mode, func(t *testing.T) {
			workspace := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "requests")
			t.Setenv("MOHUDDLE_CODEX_CWD_HELPER", test.mode)
			t.Setenv("MOHUDDLE_CODEX_CWD_LOG", logPath)
			client := New(Config{Binary: binary})
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			request := agent.TurnRequest{Workspace: workspace, Prompt: "first turn"}
			first, err := client.Run(ctx, request, func(agent.Event) {})
			if err != nil || first.Text != "answer 1" || first.SessionID != "cwd-thread" {
				t.Fatalf("first Run() = %+v, %v", first, err)
			}
			request.Prompt = "second turn"
			second, err := client.Run(ctx, request, func(agent.Event) {})
			if test.wantError == "" {
				if err != nil || second.Text != "answer 2" || second.SessionID != first.SessionID {
					t.Fatalf("second Run() = %+v, %v", second, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("second Run() error = %v, want %q", err, test.wantError)
			}
			log, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for event, want := range map[string]int{
				"initialize": test.starts, "thread/start": 1, "thread/resume": test.starts - 1,
				"accepted": test.accepted, "rejected": test.rejections,
			} {
				if got := strings.Count(string(log), event+"\n"); got != want {
					t.Errorf("%s count = %d, want %d; log:\n%s", event, got, want, log)
				}
			}
		})
	}
}

// This fake retains a thread over multiple turns. Removing and recreating its
// empty workspace reproduces a detached cwd without remounting any real drive.
func TestCodexCWDHelperProcess(t *testing.T) {
	mode := os.Getenv("MOHUDDLE_CODEX_CWD_HELPER")
	if mode == "" {
		return
	}
	logPath := os.Getenv("MOHUDDLE_CODEX_CWD_LOG")
	prior, _ := os.ReadFile(logPath)
	accepted := strings.Count(string(prior), "accepted\n")
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				CWD      string `json:"cwd"`
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			panic(err)
		}
		fmt.Fprintln(log, request.Method)
		reply := func(result any) {
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
		}
		switch request.Method {
		case "initialize":
			if !testCWDIsReachable() {
				panic("app-server inherited an invalid cwd")
			}
			reply(map[string]any{})
		case "thread/start", "thread/resume":
			if accepted > 0 && (request.Method != "thread/resume" || request.Params.ThreadID != "cwd-thread") {
				panic("recovery discarded the existing thread")
			}
			reply(map[string]any{"thread": map[string]any{"id": "cwd-thread"}})
		case "turn/start":
			if request.Params.ThreadID != "cwd-thread" {
				panic("turn started on the wrong thread")
			}
			if accepted == 1 && mode != "post-start" && (!testCWDIsReachable() || mode == "repeat" || mode == "unrelated") {
				message := "invalid cwd: No such file or directory (os error 2)"
				if mode == "unrelated" {
					message = "invalid model"
				}
				fmt.Fprintln(log, "rejected")
				_ = encoder.Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32600, "message": message}})
				continue
			}
			accepted++
			fmt.Fprintln(log, "accepted")
			turnID := fmt.Sprintf("turn-%d", accepted)
			reply(map[string]any{"turn": map[string]any{"id": turnID}})
			if accepted == 1 {
				if err := os.Remove(request.Params.CWD); err != nil {
					panic(err)
				}
				if mode != "missing" {
					if err := os.Mkdir(request.Params.CWD, 0o700); err != nil {
						panic(err)
					}
				}
				if testCWDIsReachable() {
					panic("test failed to detach app-server's cwd")
				}
			}
			_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{
				"turnId": turnID, "item": map[string]any{"type": "agentMessage", "text": fmt.Sprintf("answer %d", accepted)},
			}})
			turn := map[string]any{"id": turnID, "status": "completed"}
			if accepted == 2 && mode == "post-start" {
				turn["status"] = "failed"
				turn["error"] = "invalid cwd: No such file or directory (os error 2)"
			}
			_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": turn}})
		}
	}
	os.Exit(0)
}
