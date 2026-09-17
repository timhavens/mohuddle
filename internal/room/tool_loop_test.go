package room

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/agent/codex"
	"github.com/timhavens/mohuddle/internal/chat"
)

func toolPair(id, operation, result string, outcome agent.ToolOutcome, permanent bool) []agent.ToolObservation {
	start := agent.ToolObservation{InvocationID: id, Operation: operation, Phase: agent.ToolStarted, Outcome: agent.ToolUnknown, Complete: operation != ""}
	end := start
	end.Phase, end.Outcome, end.Result, end.NonTransient = agent.ToolCompleted, outcome, result, permanent
	return []agent.ToolObservation{start, end}
}

func emitConfirmedFailure(emit func(agent.Event), id, operation, summary string) {
	for _, observation := range toolPair(id, operation, "invalid-params", agent.ToolFailed, true) {
		kind := agent.EventToolObservation
		if observation.Phase == agent.ToolStarted {
			kind = agent.EventTool
		}
		emit(agent.Event{Type: kind, Text: summary, ToolObservation: &observation})
	}
}

func TestToolLoopEvidence(t *testing.T) {
	fail := func(id, op, result string) []agent.ToolObservation {
		return toolPair(id, op, result, agent.ToolFailed, true)
	}
	for _, tc := range []struct {
		name                 string
		build                func() []agent.ToolObservation
		recoveries, warnings int
	}{
		{"confirmed failures", func() (v []agent.ToolObservation) {
			for i := range 3 {
				v = append(v, fail(fmt.Sprint(i), "a", "error")...)
			}
			return
		}, 1, 0},
		{"different operations", func() (v []agent.ToolObservation) {
			for i := range 3 {
				v = append(v, fail(fmt.Sprint(i), fmt.Sprint(i), "error")...)
			}
			return
		}, 0, 0},
		{"different results", func() (v []agent.ToolObservation) {
			for i := range 3 {
				v = append(v, fail(fmt.Sprint(i), "a", fmt.Sprint(i))...)
			}
			return
		}, 0, 0},
		{"success is progress", func() (v []agent.ToolObservation) {
			for i := range 5 {
				v = append(v, toolPair(fmt.Sprint(i), "a", "ok", agent.ToolSucceeded, false)...)
			}
			return
		}, 0, 0},
		{"edit breaks failures", func() (v []agent.ToolObservation) {
			for i := range 5 {
				outcome := agent.ToolFailed
				op := "a"
				if i == 2 {
					outcome = agent.ToolSucceeded
					op = "edit"
				}
				v = append(v, toolPair(fmt.Sprint(i), op, "r", outcome, true)...)
			}
			return
		}, 0, 0},
		{"parallel identical failures", func() (v []agent.ToolObservation) {
			for i := range 3 {
				v = append(v, fail(fmt.Sprint(i), "a", "r")[0])
			}
			for i := 2; i >= 0; i-- {
				v = append(v, fail(fmt.Sprint(i), "a", "r")[1])
			}
			return
		}, 0, 0},
		{"duplicate lifecycle", func() (v []agent.ToolObservation) {
			p := fail("same-id", "a", "r")
			for range 4 {
				v = append(v, p[0])
			}
			for range 4 {
				v = append(v, p[1])
			}
			return
		}, 0, 0},
		{"missing starts", func() (v []agent.ToolObservation) {
			for i := range 3 {
				v = append(v, fail(fmt.Sprint(i), "a", "r")[1])
			}
			return
		}, 0, 0},
		{"missing IDs", func() (v []agent.ToolObservation) {
			for range 3 {
				v = append(v, fail("", "a", "r")...)
			}
			return
		}, 0, 0},
		{"unknown operation", func() (v []agent.ToolObservation) {
			for i := range 5 {
				op := "a"
				if i == 2 {
					op = ""
				}
				v = append(v, fail(fmt.Sprint(i), op, "r")...)
			}
			return
		}, 0, 0},
		{"generic failures warn once", func() (v []agent.ToolObservation) {
			for i := range 6 {
				v = append(v, toolPair(fmt.Sprint(i), "poll-or-shell", "error", agent.ToolFailed, false)...)
			}
			return
		}, 0, 1},
		{"failure cycle", func() (v []agent.ToolObservation) {
			for i := range 6 {
				v = append(v, fail(fmt.Sprint(i), fmt.Sprint(i%2), "r")...)
			}
			return
		}, 1, 0},
		{"unknown result", func() (v []agent.ToolObservation) {
			for i := range 5 {
				outcome := agent.ToolFailed
				if i == 2 {
					outcome = agent.ToolUnknown
				}
				v = append(v, toolPair(fmt.Sprint(i), "a", "r", outcome, true)...)
			}
			return
		}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workflowLoopMonitor{}
			recoveries, warnings := 0, 0
			for _, v := range tc.build() {
				d := m.observe(&v)
				if d.recover {
					recoveries++
				} else if d.reason != "" {
					warnings++
				}
			}
			if recoveries != tc.recoveries || warnings != tc.warnings {
				t.Fatalf("recoveries=%d warnings=%d; want %d/%d", recoveries, warnings, tc.recoveries, tc.warnings)
			}
		})
	}
}

func TestToolLoopUncertaintyAndLimits(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		m := workflowLoopMonitor{}
		v := toolPair("id", "a", "r", agent.ToolFailed, true)
		m.observe(&v[0])
		if conflict {
			v[0].Operation = "different"
			m.observe(&v[0])
		} else {
			m.observe(nil)
		}
		for i := range 4 {
			for _, v := range toolPair(fmt.Sprint(i), "a", "r", agent.ToolFailed, true) {
				if m.observe(&v).recover {
					t.Fatal("uncertain history triggered recovery")
				}
			}
		}
	}
	m := workflowLoopMonitor{}
	for i := range maxTrackedToolExecutions + 10 {
		for _, v := range toolPair(fmt.Sprint(i), "a", "ok", agent.ToolSucceeded, false) {
			m.observe(&v)
		}
	}
	if !m.disabled || len(m.executions) > maxTrackedToolExecutions {
		t.Fatalf("tracking not bounded: %d", len(m.executions))
	}
}

func TestAdvisoryCycleWarnsOnceAcrossRotations(t *testing.T) {
	m := workflowLoopMonitor{}
	warnings := 0
	for i := range 15 {
		for _, v := range toolPair(fmt.Sprint(i), fmt.Sprint(i%2), "failure", agent.ToolFailed, false) {
			d := m.observe(&v)
			if d.recover {
				t.Fatal("ambiguous cycle caused cancellation")
			}
			if d.reason != "" {
				warnings++
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("unchanged cycle emitted %d warnings", warnings)
	}
}

func TestParallelValidationIncidentPreservesWorkflow(t *testing.T) {
	data, err := os.ReadFile("../agent/codex/testdata/parallel_checks.json")
	if err != nil {
		t.Fatal(err)
	}
	var frames []struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &frames); err != nil {
		t.Fatal(err)
	}
	o, runner, peer := newTestOrchestrator(t)
	defer o.Close()
	runner.run = func(ctx context.Context, _ int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		operations := map[string]bool{}
		summaries := map[string]bool{}
		for _, frame := range frames {
			phase := agent.ToolStarted
			kind := agent.EventTool
			if frame.Method == "item/completed" {
				phase = agent.ToolCompleted
				kind = agent.EventToolObservation
			}
			v := codex.ObservationFromItem(frame.Params, request.Workspace, phase)
			var params struct {
				Item struct {
					Command string `json:"command"`
				} `json:"item"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			summary := agent.SanitizeActivitySummary(request.Workspace, "command: "+params.Item.Command)
			if phase == agent.ToolStarted {
				operations[v.Operation] = true
				summaries[summary] = true
			}
			emit(agent.Event{Type: kind, Text: summary, ToolObservation: v})
			if ctx.Err() != nil {
				t.Error("valid checks were cancelled")
			}
		}
		if len(operations) != 4 || len(summaries) != 1 {
			t.Errorf("fixture did not reproduce collision: operations=%d summaries=%d", len(operations), len(summaries))
		}
		return agent.TurnResult{Text: "all checks finished", Done: true, SessionID: "original-session"}, nil
	}
	if err := o.Post("@codex implement parallel validation regression"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var record chat.WorkflowRecord
	for time.Now().Before(deadline) {
		state, messages := o.Snapshot()
		if len(messages) > 0 {
			record = state.Workflows[messages[0].WorkflowID]
		}
		if record.State == chat.WorkflowCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if record.State != chat.WorkflowCompleted || record.RecoveryAttempts != 0 || runner.callCount() != 1 || runner.resetCount() != 0 || peer.callCount() != 0 {
		t.Fatalf("incident replay lost work: %+v calls=%d/%d resets=%d", record, runner.callCount(), peer.callCount(), runner.resetCount())
	}
	state, messages := o.Snapshot()
	if state.Sessions[chat.Codex].ID != "original-session" {
		t.Fatal("original provider session was not preserved")
	}
	for _, m := range messages {
		if strings.Contains(m.Text, "MoHuddle self-diagnosis") || strings.Contains(m.Text, "loop warning") {
			t.Fatalf("false diagnostic: %s", m.Text)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.loopMonitors) != 0 {
		t.Fatal("completed turn retained private loop history")
	}
}
