package room

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

type fakeAgent struct {
	participant chat.Participant
	mu          sync.Mutex
	calls       int
	run         func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error)
}

func (f *fakeAgent) Participant() chat.Participant { return f.participant }
func (f *fakeAgent) Close() error                  { return nil }
func (f *fakeAgent) Run(ctx context.Context, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, call, request, emit)
	}
	return agent.TurnResult{Text: string(f.participant), SessionID: string(f.participant) + "-session"}, nil
}
func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestUntargetedRoundAlternatesFourTurns(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 4)
	defer orchestrator.Close()
	if err := orchestrator.Post("solve this together"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	_, messages := orchestrator.Snapshot()
	var authors []chat.Participant
	for _, message := range messages {
		if message.Author.ValidAgent() && message.Kind == chat.MessageText {
			authors = append(authors, message.Author)
		}
	}
	want := []chat.Participant{chat.Codex, chat.Claude, chat.Codex, chat.Claude}
	if len(authors) != len(want) {
		t.Fatalf("authors=%v", authors)
	}
	for i := range want {
		if authors[i] != want[i] {
			t.Fatalf("authors=%v want=%v", authors, want)
		}
	}
}

func TestTargetedMessageOnlyRunsNamedAgent(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 4)
	defer orchestrator.Close()
	if err := orchestrator.Post("@claude answer privately"); err != nil {
		t.Fatal(err)
	}
	var lifecycle []EventType
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.Participant == chat.Claude && (event.Type == EventTurnStarted || event.Type == EventTurnFinished) {
			lifecycle = append(lifecycle, event.Type)
		}
	})
	if codexAgent.callCount() != 0 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	if len(lifecycle) != 2 || lifecycle[0] != EventTurnStarted || lifecycle[1] != EventTurnFinished {
		t.Fatalf("lifecycle=%v", lifecycle)
	}
}

func TestTargetedMessageInterruptsOnlyNamedAgent(t *testing.T) {
	started := make(chan struct{})
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 4)
	codexAgent.run = func(ctx context.Context, call int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			close(started)
			<-ctx.Done()
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{Text: "restarted", SessionID: "codex-session", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("begin"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Codex did not start")
	}
	if err := orchestrator.Post("@codex use this correction"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 0 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
}

func TestStopStillFinishesAgentLifecycle(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 4)
	codexAgent.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex wait for cancellation"); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(2 * time.Second)
	started := false
	for {
		select {
		case event := <-orchestrator.Events():
			switch event.Type {
			case EventTurnStarted:
				if event.Participant == chat.Codex {
					started = true
					orchestrator.Stop()
				}
			case EventTurnFinished:
				if event.Participant == chat.Codex {
					if !started {
						t.Fatal("turn finished before it started")
					}
					return
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for canceled turn to finish")
		}
	}
}

func TestNaturalAccessRequestIsApprovedAndRetried(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 1)
	extra := t.TempDir()
	codexAgent.run = func(ctx context.Context, call int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			return agent.TurnResult{
				Text: "I need context", SessionID: "codex-session",
				AccessRequest: &agent.AccessRequest{Path: extra, Mode: chat.AccessRead, Reason: "inspect supporting files"},
			}, nil
		}
		found := false
		for _, root := range request.ReadRoots {
			found = found || root == extra
		}
		if !found {
			t.Errorf("approved root not supplied on retry: %v", request.ReadRoots)
		}
		return agent.TurnResult{Text: "continued", SessionID: "codex-session", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("use the other directory"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.AgentEvent != nil && event.AgentEvent.Approval != nil {
			event.AgentEvent.Approval.Response <- agent.ApproveSession
		}
	})
	roomState, _ := orchestrator.Snapshot()
	found := false
	for _, grant := range roomState.Grants {
		found = found || (grant.Path == extra && grant.Participant == chat.Codex && grant.Mode == chat.AccessRead)
	}
	if !found || codexAgent.callCount() != 2 {
		t.Fatalf("grant=%v calls=%d", roomState.Grants, codexAgent.callCount())
	}
}

func TestTwoDoneResponsesStopEarly(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 8)
	codexAgent.run = doneRun(chat.Codex)
	claudeAgent.run = doneRun(chat.Claude)
	defer orchestrator.Close()
	if err := orchestrator.Post("reach consensus"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 1 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
}

func TestRevokeGrant(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t, 1)
	defer orchestrator.Close()
	extra := t.TempDir()
	orchestrator.addGrant(chat.AccessGrant{Path: extra, Mode: chat.AccessRead, Participant: chat.Claude, CreatedAt: time.Now().UTC()})
	if err := orchestrator.RevokeGrant(extra, chat.Claude); err != nil {
		t.Fatal(err)
	}
	roomState, _ := orchestrator.Snapshot()
	for _, grant := range roomState.Grants {
		if grant.Path == extra && grant.Participant == chat.Claude {
			t.Fatalf("grant was not revoked: %+v", roomState.Grants)
		}
	}
	if err := orchestrator.RevokeGrant(roomState.Workspace, ""); err == nil {
		t.Fatal("launch workspace grant was revoked")
	}
}

func doneRun(participant chat.Participant) func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: string(participant) + " done", SessionID: string(participant) + "-session", Done: true}, nil
	}
}

func newTestOrchestrator(t *testing.T, maxTurns int) (*Orchestrator, *fakeAgent, *fakeAgent) {
	t.Helper()
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), maxTurns)
	if err != nil {
		t.Fatal(err)
	}
	codexAgent := &fakeAgent{participant: chat.Codex}
	claudeAgent := &fakeAgent{participant: chat.Claude}
	orchestrator, err := New(roomState, nil, roomStore, codexAgent, claudeAgent)
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator, codexAgent, claudeAgent
}

func waitForRound(t *testing.T, events <-chan Event, inspect func(Event)) {
	t.Helper()
	timeout := time.After(4 * time.Second)
	for {
		select {
		case event := <-events:
			if inspect != nil {
				inspect(event)
			}
			if event.Type == EventError {
				t.Fatalf("orchestrator error: %v", event.Err)
			}
			if event.Type == EventRoundDone {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for round")
		}
	}
}
