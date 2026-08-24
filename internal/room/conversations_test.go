package room

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

type lifecycleTemporaryAgent struct {
	participant chat.Participant
	closed      atomic.Int32
	mu          sync.Mutex
	requests    []agent.TurnRequest
}

func (a *lifecycleTemporaryAgent) Participant() chat.Participant { return a.participant }
func (a *lifecycleTemporaryAgent) Close() error {
	a.closed.Add(1)
	return nil
}
func (a *lifecycleTemporaryAgent) Run(_ context.Context, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	call := len(a.requests)
	a.mu.Unlock()
	return agent.TurnResult{Text: "temporary answer " + string(rune('0'+call)), Done: true}, nil
}
func (a *lifecycleTemporaryAgent) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

type lifecycleTemporaryFactory struct {
	providers []chat.Participant
	mu        sync.Mutex
	created   []*lifecycleTemporaryAgent
}

func (f *lifecycleTemporaryFactory) Providers() []chat.Participant {
	return append([]chat.Participant(nil), f.providers...)
}
func (f *lifecycleTemporaryFactory) Create(_ chat.Participant, participant chat.Participant) (agent.Agent, error) {
	value := &lifecycleTemporaryAgent{participant: participant}
	f.mu.Lock()
	f.created = append(f.created, value)
	f.mu.Unlock()
	return value, nil
}
func (f *lifecycleTemporaryFactory) first() *lifecycleTemporaryAgent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) == 0 {
		return nil
	}
	return f.created[0]
}

func TestTemporaryConversationFollowupGraceAndRetirement(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	factory := &lifecycleTemporaryFactory{providers: []chat.Participant{chat.Agy}}
	orchestrator.ConfigureTemporaryAgents(factory)

	started := make(chan struct{})
	release := make(chan struct{})
	codexAgent.run = func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		close(started)
		select {
		case <-release:
			return agent.TurnResult{Text: "main work done", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}
	if err := orchestrator.Post("@codex implement the main change"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("main work did not start")
	}
	if err := orchestrator.Post("how do I inspect the conflict stats?"); err != nil {
		t.Fatal(err)
	}
	first := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	if first.Assigned != "agy-1" || !first.Temporary || first.RetireAt == nil {
		t.Fatalf("temporary answer lifecycle=%+v", first)
	}
	temporary := factory.first()
	if temporary == nil || temporary.requestCount() != 1 {
		t.Fatalf("temporary responder was not created exactly once: %+v", temporary)
	}
	temporary.mu.Lock()
	request := temporary.requests[0]
	temporary.mu.Unlock()
	if request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 || !request.Ephemeral {
		t.Fatalf("temporary request was not strictly read-only: %+v", request)
	}
	orchestrator.mu.Lock()
	if containsParticipant(orchestrator.workflowParticipantsLocked(time.Now()), "agy-1") {
		orchestrator.mu.Unlock()
		t.Fatal("temporary conversation responder leaked into the main workflow roster")
	}
	other := &chat.ConversationJob{ID: "unrelated", State: chat.ConversationFinding}
	if participant, _ := orchestrator.selectConversationResponderLocked(other, time.Now()); participant.ValidAgent() {
		orchestrator.mu.Unlock()
		t.Fatalf("temporary responder was reused across unrelated conversations: %s", participant)
	}
	orchestrator.mu.Unlock()

	if err := orchestrator.FollowUpConversation(first.ID, "and which command prints them?"); err != nil {
		t.Fatal(err)
	}
	second := waitForConversationAttempts(t, orchestrator, first.ID, 1, chat.ConversationAnswered)
	if second.Assigned != "agy-1" || !second.Temporary || second.RetireAt == nil || temporary.requestCount() != 2 {
		t.Fatalf("follow-up did not reuse grace responder: job=%+v calls=%d", second, temporary.requestCount())
	}

	orchestrator.mu.Lock()
	job := orchestrator.conversationLocked(first.ID)
	past := time.Now().UTC().Add(-time.Second)
	job.RetireAt = &past
	orchestrator.mu.Unlock()
	orchestrator.scheduleConversations()
	deadline := time.Now().Add(time.Second)
	for temporary.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if temporary.closed.Load() != 1 {
		t.Fatalf("temporary responder close count=%d", temporary.closed.Load())
	}
	roomState, messages := orchestrator.Snapshot()
	retired := roomState.Conversations[0]
	if roomState.Present("agy-1") || retired.Assigned != "" || retired.Temporary || retired.RetiredAt == nil || len(messages) < 4 {
		t.Fatalf("temporary retirement lost state/history: room=%+v messages=%+v", roomState, messages)
	}
	close(release)
}

func TestRestartReapsPersistedTemporaryAndRequeuesAttempt(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := chat.Message{ID: "source", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "what is the status?", InputIntent: chat.InputConversation, ConversationID: "conversation", CreatedAt: now}
	roomState.Members["claude-1"] = true
	roomState.Sessions["claude-1"] = chat.AgentSession{ID: "stale-provider-session"}
	deadline := now.Add(time.Minute)
	roomState.Conversations = []chat.ConversationJob{{
		ID: "conversation", SourceSequence: 1, State: chat.ConversationAnswering, Class: chat.ConversationQuick,
		Assigned: "claude-1", Temporary: true, CreatedAt: now, UpdatedAt: now, StartedAt: &now, Deadline: &deadline, LastActivityAt: now,
		Attempts: []chat.ConversationAttempt{{Participant: "claude-1", Provider: chat.Claude, StartedAt: now, Deadline: deadline}},
	}, {
		ID: "exhausted", SourceSequence: 2, State: chat.ConversationRetrying, Class: chat.ConversationQuick,
		Assigned: chat.Claude, CreatedAt: now, UpdatedAt: now, StartedAt: &now, Deadline: &deadline, LastActivityAt: now,
		Attempts: []chat.ConversationAttempt{
			{Participant: chat.Codex, Provider: chat.Codex, StartedAt: now, Deadline: deadline, CompletedAt: &now, Error: "first failed"},
			{Participant: chat.Claude, Provider: chat.Claude, StartedAt: now, Deadline: deadline},
		},
	}}
	temporary := &lifecycleTemporaryAgent{participant: "claude-1"}
	orchestrator, err := New(roomState, []chat.Message{source}, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude}, temporary)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	restored, _ := orchestrator.Snapshot()
	job := restored.Conversations[0]
	if temporary.closed.Load() != 1 || restored.Present("claude-1") || job.State != chat.ConversationWaiting || job.Assigned != "" || job.Temporary || job.RetiredAt == nil {
		t.Fatalf("restart did not reap/requeue temporary responder: room=%+v close=%d", restored, temporary.closed.Load())
	}
	exhausted := restored.Conversations[1]
	if exhausted.State != chat.ConversationNeedsAttention || exhausted.Assigned != "" || exhausted.TerminalReason == "" || exhausted.Attempts[1].CompletedAt == nil {
		t.Fatalf("restart launched a third automatic attempt: %+v", exhausted)
	}
	time.Sleep(20 * time.Millisecond)
	if job = snapshotRoom(orchestrator).Conversations[0]; job.State != chat.ConversationWaiting {
		t.Fatalf("conversation scheduled before runtime configuration: %+v", job)
	}
}

func TestQueuedConversationDeadlineAndGlobalStopAreTerminal(t *testing.T) {
	t.Run("queue deadline", func(t *testing.T) {
		orchestrator, _, _ := newTestOrchestrator(t)
		defer orchestrator.Close()
		orchestrator.ConfigureTemporaryAgents(nil)
		orchestrator.mu.Lock()
		for participant := range orchestrator.room.Members {
			orchestrator.room.Members[participant] = false
		}
		orchestrator.mu.Unlock()
		if err := orchestrator.Post("what is the current status?"); err != nil {
			t.Fatal(err)
		}
		waitForConversationState(t, orchestrator, chat.ConversationWaiting)
		orchestrator.mu.Lock()
		past := time.Now().UTC().Add(-time.Second)
		orchestrator.room.Conversations[0].Deadline = &past
		orchestrator.mu.Unlock()
		orchestrator.scheduleConversations()
		job := waitForConversationState(t, orchestrator, chat.ConversationNeedsAttention)
		if job.TerminalReason != "response deadline expired" || job.QueuePosition != 0 {
			t.Fatalf("expired queued conversation=%+v", job)
		}
	})

	t.Run("global stop", func(t *testing.T) {
		orchestrator, codexAgent, _ := newTestOrchestrator(t)
		defer orchestrator.Close()
		orchestrator.ConfigureTemporaryAgents(nil)
		started := make(chan struct{})
		stopped := make(chan struct{})
		codexAgent.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return agent.TurnResult{}, ctx.Err()
		}
		if err := orchestrator.Post("what is the current status?"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("conversation did not start")
		}
		orchestrator.Stop()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("global stop did not cancel conversation turn")
		}
		job := waitForConversationState(t, orchestrator, chat.ConversationCancelled)
		if job.TerminalReason != "cancelled by the human" || orchestrator.HasActiveWork() {
			t.Fatalf("global stop state=%+v active=%v", job, orchestrator.HasActiveWork())
		}
	})
}

func TestTemporaryConversationSkipsUnavailableProvider(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()

	retryAt := time.Now().UTC().Add(time.Hour)
	orchestrator.mu.Lock()
	for participant := range orchestrator.room.Members {
		orchestrator.room.Members[participant] = false
	}
	orchestrator.room.Availability[chat.Agy] = chat.ParticipantAvailability{
		Reason: "provider cooldown", Source: "detected", DetectedAt: time.Now().UTC(), RetryAt: &retryAt,
	}
	orchestrator.mu.Unlock()
	factory := &lifecycleTemporaryFactory{providers: []chat.Participant{chat.Agy, chat.Copilot}}
	orchestrator.ConfigureTemporaryAgents(factory)

	if err := orchestrator.Post("what is the current status?"); err != nil {
		t.Fatal(err)
	}
	job := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	if job.Assigned != "copilot-1" || len(job.Attempts) != 1 || job.Attempts[0].Provider != chat.Copilot {
		t.Fatalf("temporary responder ignored provider cooldown: %+v", job)
	}
}

func TestConversationRetryAndLateResultHaveOneVisibleOutcome(t *testing.T) {
	t.Run("alternate provider retry", func(t *testing.T) {
		orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
		defer orchestrator.Close()
		orchestrator.ConfigureTemporaryAgents(nil)
		codexAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{}, errors.New("transient provider failure")
		}
		claudeAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{Text: "alternate provider answer", Done: true}, nil
		}
		if err := orchestrator.Post("what is the retry status?"); err != nil {
			t.Fatal(err)
		}
		job := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
		if len(job.Attempts) != 2 || job.Attempts[0].Provider != chat.Codex || job.Attempts[1].Provider != chat.Claude || job.AnswerSequence == 0 {
			t.Fatalf("alternate retry lifecycle=%+v", job)
		}
	})

	t.Run("complete outage", func(t *testing.T) {
		orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
		defer orchestrator.Close()
		orchestrator.ConfigureTemporaryAgents(nil)
		fail := func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{}, errors.New("provider unavailable")
		}
		codexAgent.run = fail
		claudeAgent.run = fail
		if err := orchestrator.Post("what is the outage status?"); err != nil {
			t.Fatal(err)
		}
		job := waitForConversationState(t, orchestrator, chat.ConversationNeedsAttention)
		if len(job.Attempts) != 2 || job.TerminalReason == "" || job.AnswerSequence != 0 {
			t.Fatalf("outage lifecycle=%+v", job)
		}
	})

	t.Run("late answer after deadline", func(t *testing.T) {
		orchestrator, codexAgent, _ := newTestOrchestrator(t)
		defer orchestrator.Close()
		orchestrator.ConfigureTemporaryAgents(nil)
		started := make(chan struct{})
		release := make(chan struct{})
		codexAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			close(started)
			<-release
			return agent.TurnResult{Text: "late answer must not publish", Done: true}, nil
		}
		if err := orchestrator.Post("what is the deadline status?"); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("conversation attempt did not start")
		}
		orchestrator.mu.Lock()
		past := time.Now().UTC().Add(-time.Second)
		orchestrator.room.Conversations[0].Deadline = &past
		orchestrator.mu.Unlock()
		orchestrator.scheduleConversations()
		waitForConversationState(t, orchestrator, chat.ConversationNeedsAttention)
		close(release)
		orchestrator.wg.Wait()
		roomState, messages := orchestrator.Snapshot()
		job := roomState.Conversations[0]
		if job.State != chat.ConversationNeedsAttention || job.AnswerSequence != 0 || len(messages) != 1 {
			t.Fatalf("late answer changed terminal result: job=%+v messages=%+v", job, messages)
		}
	})
}

func waitForConversationAttempts(t *testing.T, orchestrator *Orchestrator, id string, attempts int, state chat.ConversationState) chat.ConversationJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		roomState, _ := orchestrator.Snapshot()
		for _, job := range roomState.Conversations {
			if job.ID == id && len(job.Attempts) >= attempts && job.State == state {
				return job
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for conversation %s attempts=%d state=%s", id, attempts, state)
	return chat.ConversationJob{}
}
