package room

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
)

type fakeAgent struct {
	participant chat.Participant
	mu          sync.Mutex
	calls       int
	configured  chat.AgentSettings
	resetConfig bool
	run         func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error)
}

func (f *fakeAgent) Participant() chat.Participant { return f.participant }
func (f *fakeAgent) Close() error                  { return nil }
func (f *fakeAgent) Configure(value chat.AgentSettings) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured = value
	return f.resetConfig
}
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

func TestMaterialDisagreementPausesAndPersistsUntilUserActs(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 8)
	codexAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "This approach is unsafe", SessionID: "codex-session", Disagrees: true, ConflictReason: "unsafe migration order"}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("review the migration"); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
			if event.Type == EventConflict {
				goto paused
			}
		case <-timeout:
			t.Fatal("timed out waiting for conflict")
		}
	}

paused:
	if claudeAgent.callCount() != 0 {
		t.Fatalf("Claude ran after conflict: %d calls", claudeAgent.callCount())
	}
	roomState, _ := orchestrator.Snapshot()
	if roomState.Conflict == nil || roomState.Conflict.RaisedBy != chat.Codex || roomState.Conflict.Reason != "unsafe migration order" {
		t.Fatalf("conflict=%+v", roomState.Conflict)
	}
	if err := orchestrator.Post("use the safer ordering"); err != nil {
		t.Fatal(err)
	}
	roomState, _ = orchestrator.Snapshot()
	if roomState.Conflict != nil {
		t.Fatalf("user direction did not clear conflict: %+v", roomState.Conflict)
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

func TestSettingsRequireFullAcknowledgementAndPersistRoomOverride(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t, 1)
	defer orchestrator.Close()
	preferences, err := appsettings.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	value := chat.AgentSettings{Model: "custom", Effort: "high", Permissions: chat.PermissionFull}
	if err := orchestrator.SetAgentSettings(chat.Codex, value, false); err == nil {
		t.Fatal("full access was accepted without acknowledgement")
	}
	if err := orchestrator.AcknowledgeFullAccess(); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetAgentSettings(chat.Codex, value, false); err != nil {
		t.Fatal(err)
	}
	configured := orchestrator.EffectiveSettings()[chat.Codex]
	if configured != value {
		t.Fatalf("effective=%+v want=%+v", configured, value)
	}
	roomState, _ := orchestrator.Snapshot()
	if got := roomState.Settings[chat.Codex]; got != value {
		t.Fatalf("room override=%+v want=%+v", got, value)
	}
}

func TestProviderSessionResetReplaysRoomTranscript(t *testing.T) {
	orchestrator, _, claudeAgent := newTestOrchestrator(t, 1)
	defer orchestrator.Close()
	claudeAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "first response", SessionID: "old-session", Done: true}, nil
	}
	if err := orchestrator.Post("@claude first question"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	claudeAgent.mu.Lock()
	claudeAgent.resetConfig = true
	claudeAgent.mu.Unlock()
	if err := orchestrator.SetAgentSettings(chat.Claude, chat.AgentSettings{Model: "opus", Permissions: chat.PermissionWorkspace}, false); err != nil {
		t.Fatal(err)
	}
	roomState, _ := orchestrator.Snapshot()
	if session := roomState.Sessions[chat.Claude]; session.ID != "" || session.Cursor != 0 {
		t.Fatalf("session was not reset: %+v", session)
	}
	requestSeen := make(chan agent.TurnRequest, 1)
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		requestSeen <- request
		return agent.TurnResult{Text: "second response", SessionID: "new-session", Done: true}, nil
	}
	if err := orchestrator.Post("@claude second question"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	request := <-requestSeen
	if !strings.Contains(request.Prompt, "first question") || !strings.Contains(request.Prompt, "first response") || !strings.Contains(request.Prompt, "second question") {
		t.Fatalf("reset transcript was incomplete: %s", request.Prompt)
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
