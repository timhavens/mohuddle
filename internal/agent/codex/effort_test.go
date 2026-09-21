package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func effortTestClient(t *testing.T, mode string, resumed bool) (*Client, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	dir := t.TempDir()
	wrapper, log := filepath.Join(dir, "fake-codex"), filepath.Join(dir, "requests.jsonl")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexEffortHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_EFFORT_HELPER", mode)
	t.Setenv("MOHUDDLE_EFFORT_LOG", log)
	config := Config{Binary: wrapper}
	if resumed {
		config.SessionID = "same-thread"
	}
	client := New(config)
	t.Cleanup(func() { _ = client.Close() })
	return client, dir, log
}

func effortRequests(t *testing.T, path, method string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var requests []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var request map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if request["method"] == method {
			requests = append(requests, request["params"].(map[string]any))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return requests
}

func TestCodexEffortRestoresNativeDefaultWithoutResetAndDoesNotInventConfirmation(t *testing.T) {
	for _, mode := range []string{"confirmed", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			client, dir, log := effortTestClient(t, mode, false)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			wanted := []string{"low", "medium", "max", "medium"}
			for i, effort := range []string{"low", "", "max", "auto"} {
				settings := chat.AgentSettings{Model: "test-model", Effort: effort, Permissions: chat.PermissionWorkspace}
				if client.Configure(settings) {
					t.Fatal("effort reset the native session")
				}
				result, err := client.Run(ctx, agent.TurnRequest{Workspace: dir, Prompt: "Bounded task", Settings: settings}, func(agent.Event) {})
				if err != nil || result.Text != "finished" || result.SessionID != "same-thread" {
					t.Fatalf("turn %d: %+v %v", i, result, err)
				}
				if (mode == "confirmed" && result.RuntimeEffort != wanted[i]) || (mode == "unconfirmed" && result.RuntimeEffort != "") {
					t.Fatalf("turn %d provider confirmation: %+v", i, result)
				}
			}
			turns := effortRequests(t, log, "turn/start")
			var got []string
			for _, turn := range turns {
				got = append(got, turn["effort"].(string))
				if turn["threadId"] != "same-thread" {
					t.Fatal("native thread changed")
				}
			}
			if !slices.Equal(got, wanted) || len(effortRequests(t, log, "thread/start")) != 1 || len(effortRequests(t, log, "config/read")) != 0 {
				t.Fatalf("native effort sequence = %v", got)
			}
		})
	}
}

func TestCodexEffortOnResumedThreadResolvesDefaultOrRefusesToStart(t *testing.T) {
	for _, tc := range []struct{ mode, wanted string }{{"config", "medium"}, {"catalog", "high"}, {"unavailable", ""}} {
		t.Run(tc.mode, func(t *testing.T) {
			client, dir, log := effortTestClient(t, tc.mode, true)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := client.Run(ctx, agent.TurnRequest{Workspace: dir, Prompt: "Resume at provider default", Settings: chat.AgentSettings{Model: "test-model", Effort: "auto", Permissions: chat.PermissionWorkspace}}, func(agent.Event) {})
			turns := effortRequests(t, log, "turn/start")
			if tc.wanted == "" {
				var effortErr *chat.EffortError
				if !errors.As(err, &effortErr) || len(turns) != 0 {
					t.Fatalf("unresolved default started a turn: %v %+v", err, turns)
				}
			} else if err != nil || len(turns) != 1 || turns[0]["effort"] != tc.wanted || result.RuntimeEffort != tc.wanted {
				t.Fatalf("native default: turns=%+v result=%+v err=%v", turns, result, err)
			}
			if len(effortRequests(t, log, "thread/resume")) != 1 || len(effortRequests(t, log, "config/read")) != 1 {
				t.Fatal("resumed thread used its persisted override as the default")
			}
		})
	}
}

func TestCodexEffortCatalogIncludesLaterPagesAndKnownEmptySupport(t *testing.T) {
	client, _, _ := effortTestClient(t, "catalog", false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	models, err := client.Models(ctx)
	if err != nil || len(models) != 2 || !models[0].EffortsKnown || len(models[0].Efforts) != 0 || models[1].ID != "test-model" || !models[1].EffortsKnown || !slices.Equal(models[1].Efforts, []string{"low", "high"}) {
		t.Fatalf("paginated capabilities: %+v %v", models, err)
	}
}

// A native transport fixture records the actual RPCs and deliberately reports a
// persisted low override on resume, which must never become the auto baseline.
func TestCodexEffortHelperProcess(t *testing.T) {
	mode := os.Getenv("MOHUDDLE_EFFORT_HELPER")
	if mode == "" {
		return
	}
	log, err := os.OpenFile(os.Getenv("MOHUDDLE_EFFORT_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	scanner, encoder := bufio.NewScanner(os.Stdin), json.NewEncoder(os.Stdout)
	turn := 0
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(3)
		}
		_, _ = log.Write(append(append([]byte{}, scanner.Bytes()...), '\n'))
		var result any = map[string]any{}
		switch request.Method {
		case "initialized":
			continue
		case "initialize":
		case "thread/start", "thread/resume":
			effort := "medium"
			if request.Method == "thread/resume" {
				effort = "low"
			}
			result = map[string]any{"thread": map[string]any{"id": "same-thread"}, "model": "test-model", "reasoningEffort": effort}
		case "config/read":
			config := map[string]any{"model": "test-model"}
			if mode == "config" {
				config["model_reasoning_effort"] = "medium"
			}
			result = map[string]any{"config": config}
		case "model/list":
			if mode == "unavailable" {
				result = map[string]any{"data": []any{}}
			} else if request.Params["cursor"] == "next" {
				result = map[string]any{"data": []any{map[string]any{"id": "test-model", "isDefault": true, "defaultReasoningEffort": "high", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "high"}}}}}
			} else {
				result = map[string]any{"data": []any{map[string]any{"id": "other-model", "supportedReasoningEfforts": []any{}}}, "nextCursor": "next"}
			}
		case "turn/start":
			turn++
			turnID := fmt.Sprintf("turn-%d", turn)
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]any{"id": turnID}}})
			if mode != "unconfirmed" {
				_ = encoder.Encode(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": "same-thread", "threadSettings": map[string]any{"model": "test-model", "effort": request.Params["effort"]}}})
			}
			_ = encoder.Encode(deltaMessage("same-thread", turnID, "answer", "finished"))
			_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "same-thread", "turn": map[string]any{"id": turnID, "status": "completed"}}})
			continue
		default:
			os.Exit(4)
		}
		_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
	}
	_ = log.Close()
	os.Exit(0)
}
