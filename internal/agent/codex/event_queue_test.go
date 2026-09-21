package codex

import (
	"context"
	"encoding/json"
	"errors"
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

func deltaMessage(thread, turn, item, text string) rpcMessage {
	p, _ := json.Marshal(textDelta{ThreadID: thread, TurnID: turn, ItemID: item, Delta: text})
	return rpcMessage{Method: "item/agentMessage/delta", Params: p}
}

func TestNotificationQueueBurstPreservesBoundaries(t *testing.T) {
	q := newNotificationQueue()
	for i := 0; i < 10000; i++ {
		if err := q.push(deltaMessage("thread", "turn", "a", "x")); err != nil {
			t.Fatal(err)
		}
	}
	boundary := rpcMessage{Method: "item/completed", Params: json.RawMessage(`{"item":{"id":"a"}}`)}
	if err := q.push(boundary); err != nil {
		t.Fatal(err)
	}
	for _, m := range []rpcMessage{deltaMessage("thread", "turn", "a", "y"), deltaMessage("thread", "turn", "b", "z"), deltaMessage("thread", "next", "b", "n")} {
		if err := q.push(m); err != nil {
			t.Fatal(err)
		}
	}
	first, ok, err := q.pop()
	var text textDelta
	_ = json.Unmarshal(first.Params, &text)
	if !ok || err != nil || text.Delta != strings.Repeat("x", 10000) {
		t.Fatal("lost streamed text")
	}
	got, _, _ := q.pop()
	if got.Method != "item/completed" {
		t.Fatal("crossed lifecycle boundary")
	}
	for _, want := range []string{"y", "z", "n"} {
		got, _, _ := q.pop()
		_ = json.Unmarshal(got.Params, &text)
		if text.Delta != want {
			t.Fatal("crossed identity boundary")
		}
	}
	if q.coalesced != 9999 {
		t.Fatalf("coalesced=%d", q.coalesced)
	}
}

func TestNotificationQueueLimitsAndEOF(t *testing.T) {
	for _, bytesLimit := range []bool{false, true} {
		q := newNotificationQueue()
		m := rpcMessage{Method: "item/started", Params: json.RawMessage(`{}`)}
		count := maxQueuedEvents + 1
		if bytesLimit {
			m.Params = make([]byte, maxQueuedBytes/2)
			count = 3
		}
		var err error
		for i := 0; i < count; i++ {
			if err = q.push(m); err != nil {
				break
			}
		}
		var overflow *agent.EventQueueOverflowError
		if !errors.As(err, &overflow) || q.highBytes > maxQueuedBytes || q.highEntries > maxQueuedEvents {
			t.Fatalf("unbounded or untyped failure: %v", err)
		}
		if _, _, err = q.pop(); !errors.As(err, &overflow) {
			t.Fatal("overflow was hidden")
		}
	}
	q := newNotificationQueue()
	_ = q.push(rpcMessage{Method: "turn/completed"})
	q.end(errors.New("EOF"))
	if m, ok, err := q.pop(); !ok || err != nil || m.Method != "turn/completed" {
		t.Fatal("EOF discarded completion")
	}
	if _, _, err := q.pop(); err == nil {
		t.Fatal("EOF did not release consumer")
	}
}

func TestReaderRoutesRPCWithoutConsumingNotifications(t *testing.T) {
	c := New(Config{})
	response := make(chan callResult, 1)
	c.pending.Store("9", response)
	var input strings.Builder
	for i := 0; i < 1000; i++ {
		input.WriteString(`{"method":"ignored/reasoning","params":{}}` + "\n")
		p, _ := json.Marshal(deltaMessage("t", "v", "i", "x"))
		input.Write(p)
		input.WriteByte('\n')
	}
	input.WriteString("{\"id\":9,\"result\":{\"ok\":true}}\n")
	c.readLoop(strings.NewReader(input.String()))
	select {
	case r := <-response:
		if r.err != nil {
			t.Fatal(r.err)
		}
	default:
		t.Fatal("RPC blocked behind stream")
	}
	if c.events.highEntries != 1 {
		t.Fatal("irrelevant events were queued")
	}
}

func TestClientBurstWithSlowConsumerAndStaleTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "codex")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	var messages []rpcMessage
	for i := 0; i < 512; i++ {
		messages = append(messages, deltaMessage("codex-thread", "codex-turn", "answer", "x"))
	}
	messages = append(messages, deltaMessage("old-thread", "old-turn", "answer", "STALE"))
	encoded, _ := json.Marshal(messages)
	t.Setenv("MOHUDDLE_CODEX_MCP_EVENTS", string(encoded))
	c := New(Config{Binary: wrapper})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var streamed strings.Builder
	result, err := c.Run(ctx, agent.TurnRequest{Workspace: dir, Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace}}, func(e agent.Event) {
		if e.Approval != nil {
			e.Approval.Response <- agent.ApproveOnce
		}
		if e.Type == agent.EventDelta {
			streamed.WriteString(e.Text)
			time.Sleep(time.Millisecond)
		}
	})
	if err != nil || result.Text != "hello from codex" || streamed.String() != strings.Repeat("x", 512)+"hello from codex" {
		t.Fatalf("burst failure: %v final=%q streamed=%d", err, result.Text, streamed.Len())
	}
}

func TestClientCancellationOverflowAndConsecutiveTurns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper")
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "codex")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestCodexHelperProcess -- \"$@\"\n", os.Args[0])), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_CODEX_HELPER", "1")
	request := agent.TurnRequest{Workspace: dir, Settings: chat.AgentSettings{Model: "test-model", Effort: "high", Permissions: chat.PermissionWorkspace}}
	for _, mode := range []string{"cancel", "overflow", "repeat"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MOHUDDLE_CODEX_STREAM_MODE", mode)
			c := New(Config{Binary: wrapper})
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := c.Run(ctx, request, func(e agent.Event) {
				if mode == "cancel" && e.Type == agent.EventDelta {
					cancel()
				}
				if mode == "overflow" && e.Type == agent.EventStatus {
					// Hold the turn consumer while the independent reader reaches its
					// hard bound and terminates the transport.
					until := time.Now().Add(2 * time.Second)
					for !c.processExited.Load() && time.Now().Before(until) {
						time.Sleep(time.Millisecond)
					}
				}
			})
			switch mode {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			case "overflow":
				var overflow *agent.EventQueueOverflowError
				if !errors.As(err, &overflow) {
					t.Fatalf("overflow: %v", err)
				}
				if c.started {
					t.Fatal("failed transport was retained")
				}
			case "repeat":
				if err != nil || result.Text != "current" {
					t.Fatalf("first: %q %v", result.Text, err)
				}
				result, err = c.Run(ctx, request, func(agent.Event) {})
				if err != nil || result.Text != "current" {
					t.Fatalf("next turn inherited stale text: %q %v", result.Text, err)
				}
			}
		})
	}
}
