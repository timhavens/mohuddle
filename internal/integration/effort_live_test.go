package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/agent/codex"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
)

// This opt-in exercise creates its own room and disposable workspace. It does
// not connect to a running room or change provider configuration files.
func TestLiveChatGPTEffortSmallEditAndConcurrencyDebug(t *testing.T) {
	if os.Getenv("MOHUDDLE_LIVE_EFFORT") != "1" {
		t.Skip("set MOHUDDLE_LIVE_EFFORT=1 for isolated authenticated Codex effort checks")
	}
	workspace := t.TempDir()
	files := map[string]string{
		"README.md": "# Sample\n\nKeep this body unchanged.\n",
		"go.mod":    "module effortfixture\n\ngo 1.24\n",
		"cache.go": `package effortfixture
import "sync"
type Cache struct { mu sync.Mutex; values map[string]string }
func New() *Cache { return &Cache{values:map[string]string{}} }
func (c *Cache) Get(key string, load func()(string,error)) (string,error) {
 c.mu.Lock(); defer c.mu.Unlock()
 if value,ok:=c.values[key]; ok { return value,nil }
 value,err:=load()
 c.values[key]=value
 return value,err
}
`,
		"cache_test.go": `package effortfixture
import("errors";"sync";"sync/atomic";"testing";"time")
func TestDifferentKeysDoNotBlock(t *testing.T) {
 c:=New(); entered,release,done:=make(chan struct{}),make(chan struct{}),make(chan struct{})
 go func(){ c.Get("slow",func()(string,error){ close(entered); <-release; return "slow",nil }) }()
 <-entered; defer close(release)
 go func(){ c.Get("fast",func()(string,error){ return "fast",nil }); close(done) }()
 select { case <-done: case <-time.After(time.Second): t.Fatal("one loader blocks unrelated keys") }
}
func TestConcurrentSameKeyLoadsOnce(t *testing.T) {
 c:=New(); var calls atomic.Int32; var wg sync.WaitGroup
 entered,release:=make(chan struct{}),make(chan struct{})
 load:=func()(string,error){ if calls.Add(1)==1 {close(entered)}; <-release; return "value",nil }
 for i:=0;i<20;i++ { wg.Add(1); go func(){defer wg.Done(); v,e:=c.Get("key",load); if e!=nil||v!="value" {t.Errorf("result %q %v",v,e)} }() }
 <-entered; close(release); wg.Wait(); if calls.Load()!=1 {t.Fatalf("loads=%d",calls.Load())}
}
func TestErrorsAreRetryable(t *testing.T) {
 c:=New(); boom:=errors.New("temporary"); c.Get("key",func()(string,error){return "",boom})
 value,err:=c.Get("key",func()(string,error){return "recovered",nil})
 if err!=nil || value!="recovered" {t.Fatalf("cached failed load: %q %v",value,err)}
}
func TestLoaderMayReadDifferentKey(t *testing.T) {
 c:=New(); done:=make(chan struct{})
 go func(){ c.Get("outer",func()(string,error){return c.Get("inner",func()(string,error){return "nested",nil})}); close(done) }()
 select {case <-done: case <-time.After(time.Second):t.Fatal("loader deadlocks on another key")}
}
`,
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stateStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := stateStore.Create(workspace, 1)
	if err != nil {
		t.Fatal(err)
	}
	peer := codex.New(codex.Config{})
	o, err := room.New(state, nil, stateStore, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := o.SetAgentSettings(chat.Codex, chat.AgentSettings{Model: os.Getenv("MOHUDDLE_EFFORT_MODEL"), Effort: "max", Permissions: chat.PermissionWorkspace}, false); err != nil {
		t.Fatal(err)
	}
	o.UpdateChatGPTState(chat.ChatGPTState{Enabled: true, Connected: true, LeaseUntil: time.Now().Add(10 * time.Minute), ExpiresAt: time.Now().Add(time.Hour), ExchangesRemaining: 32})
	go func() {
		for {
			select {
			case event := <-o.Events():
				if event.AgentEvent != nil && event.AgentEvent.Approval != nil {
					select {
					case event.AgentEvent.Approval.Response <- agent.ApproveOnce:
					case <-t.Context().Done():
						return
					}
				}
			case <-t.Context().Done():
				return
			}
		}
	}()
	for i, task := range []struct{ effort, prompt string }{
		{"low", "Edit only README.md: change the title from Sample to Effort smoke test. Preserve its body exactly. Verify and finish with a brief public answer. This is a disposable fixture; no research, web, MCP, delegation, commits, or other edits are needed."},
		{"high", "Fix only cache.go. Its loaders currently serialize unrelated keys, deadlock on nested reads of another key, and cache failed loads. Preserve same-key single-flight behavior: concurrent successful requests share one load. Do not hold the cache mutex while invoking load; failures must allow a later retry. Keep the public API and existing tests unchanged. Run go test -race ./... and report results briefly. Only these disposable fixture files are relevant; no research, web, MCP, delegation, dependency changes, or commits."},
	} {
		start := time.Now()
		message, _, err := o.RequestChatGPTWork(task.prompt, chat.Codex, 0, chat.RouteMetadata{MessageID: fmt.Sprintf("live-effort-%d", i)}, chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: task.effort}, Reason: "Bounded effort validation"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		var record chat.WorkflowRecord
		for {
			current, _ := o.Snapshot()
			record = current.Workflows[message.WorkflowID]
			if record.State.Terminal() {
				break
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s task timed out: %+v", task.effort, record)
			}
		}
		cancel()
		current, _ := o.Snapshot()
		status := record.EffortStatus[chat.Codex]
		if record.State != chat.WorkflowCompleted || status.AppliedEffort != task.effort || current.Settings[chat.Codex].Effort != "max" {
			t.Fatalf("effort workflow: %+v", record)
		}
		t.Logf("effort=%s elapsed=%s applied=%s provider_model=%q provider_effort=%q session=%s; adapter does not expose token totals", task.effort, time.Since(start).Round(time.Millisecond), status.AppliedEffort, status.ReportedModel, status.ReportedEffort, peer.SessionID())
	}
	data, err := os.ReadFile(filepath.Join(workspace, "README.md"))
	if err != nil || string(data) != "# Effort smoke test\n\nKeep this body unchanged.\n" {
		t.Fatalf("small-edit quality: %q %v", data, err)
	}
	for _, path := range []string{"go.mod", "cache_test.go"} {
		data, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || string(data) != files[path] {
			t.Fatalf("agent changed verification fixture %s", path)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-race", "./...")
	command.Dir = workspace
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "ok") {
		t.Fatalf("debug quality: %s %v", output, err)
	}
	t.Log("quality verified: exact title-only edit; independent race-enabled concurrency and error-retry tests passed")
}
