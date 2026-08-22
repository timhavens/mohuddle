package room

import (
	"context"
	"fmt"
	"slices"
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
	return agent.TurnResult{Text: string(f.participant), SessionID: string(f.participant) + "-session", Done: true}, nil
}
func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestUntargetedAgentsRunConcurrentlyReadOnlyAndCrossReview(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	requests := make(chan agent.TurnRequest, 4)
	for _, fake := range []*fakeAgent{codexAgent, claudeAgent} {
		participant := fake.participant
		fake.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			requests <- request
			if call == 1 {
				started <- participant
				<-release
			}
			return agent.TurnResult{Text: string(participant) + " wave", SessionID: string(participant) + "-session", Done: true}, nil
		}
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("solve this together"); err != nil {
		t.Fatal(err)
	}
	seen := map[chat.Participant]bool{}
	for len(seen) < 2 {
		select {
		case participant := <-started:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("agents did not overlap in the first wave")
		}
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	close(requests)
	firstWave := 0
	reviewWave := 0
	for request := range requests {
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("untargeted permission=%s", request.Settings.Permissions)
		}
		if strings.Contains(request.SystemPrompt, "Independently analyze") {
			firstWave++
			if strings.Contains(request.Prompt, "codex wave") || strings.Contains(request.Prompt, "claude wave") {
				t.Errorf("same-wave response leaked into independent prompt: %s", request.Prompt)
			}
		}
		if strings.Contains(request.SystemPrompt, "Review the other agents") {
			reviewWave++
			if !strings.Contains(request.Prompt, "codex wave") || !strings.Contains(request.Prompt, "claude wave") {
				t.Errorf("cross-review prompt did not contain both first-wave responses: %s", request.Prompt)
			}
		}
	}
	if firstWave != 2 {
		t.Fatalf("independent first-wave requests=%d", firstWave)
	}
	if reviewWave != 2 {
		t.Fatalf("cross-review requests=%d", reviewWave)
	}
}

func TestTargetedMessageRunsEditorThenReadOnlyReviewer(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	var codexPermission, claudePermission chat.PermissionProfile
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		codexPermission = request.Settings.Permissions
		return agent.TurnResult{Text: "review approved", Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		claudePermission = request.Settings.Permissions
		return agent.TurnResult{Text: "edited", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@claude answer this"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 1 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	if claudePermission != chat.PermissionWorkspace || codexPermission != chat.PermissionReadOnly {
		t.Fatalf("editor=%s reviewer=%s", claudePermission, codexPermission)
	}
}

func TestMarkerOnlyCompletionDoesNotCreatePublicPlaceholder(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 3)
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	codexAgent.run = func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{SessionID: "codex-session", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex remain quiet"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	roomState, messages := orchestrator.Snapshot()
	for _, message := range messages {
		if message.Author == chat.Codex && message.Kind == chat.MessageText {
			t.Fatalf("marker-only turn created public message: %+v", message)
		}
	}
	if roomState.Sessions[chat.Codex].ID != "codex-session" || roomState.Sessions[chat.Codex].Cursor == 0 {
		t.Fatalf("quiet completion did not persist session progress: %+v", roomState.Sessions[chat.Codex])
	}
}

func TestTargetedEditorCorrectsWorkUntilReviewersAgree(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	codexAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionWorkspace {
			t.Errorf("editor permission=%s", request.Settings.Permissions)
		}
		return agent.TurnResult{Text: fmt.Sprintf("edit %d", call), Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("reviewer permission=%s", request.Settings.Permissions)
		}
		if call == 1 {
			return agent.TurnResult{Text: "needs correction", Done: true, Disagrees: true, ConflictReason: "test is missing"}, nil
		}
		if !strings.Contains(request.Prompt, "edit 2") {
			t.Errorf("second review did not receive correction: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "approved", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex implement it"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	roomState, _ := orchestrator.Snapshot()
	if roomState.Conflict != nil {
		t.Fatalf("resolved review left conflict=%+v", roomState.Conflict)
	}
}

func TestNewSteeringCancelsAllAgentsAndPreservesInterruptedDrafts(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	started := make(chan chat.Participant, 2)
	for _, fake := range []*fakeAgent{codexAgent, claudeAgent} {
		participant := fake.participant
		fake.run = func(ctx context.Context, call int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
			if call == 1 {
				emit(agent.Event{Type: agent.EventDelta, Agent: participant, Text: "partial " + string(participant)})
				started <- participant
				<-ctx.Done()
				return agent.TurnResult{}, ctx.Err()
			}
			return agent.TurnResult{Text: string(participant) + " restarted", SessionID: string(participant) + "-session", Done: true}, nil
		}
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("begin"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("agents did not start")
		}
	}
	if err := orchestrator.Post("@codex use this correction"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	_, messages := orchestrator.Snapshot()
	interrupted := map[chat.Participant]bool{}
	for _, message := range messages {
		if message.Kind == chat.MessageInterrupted {
			interrupted[message.Author] = true
		}
	}
	if !interrupted[chat.Codex] || !interrupted[chat.Claude] {
		t.Fatalf("interrupted drafts=%v", interrupted)
	}
}

func TestStopStillFinishesAgentLifecycle(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 4)
	codexAgent.run = func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		emit(agent.Event{Type: agent.EventDelta, Agent: chat.Codex, Text: "unfinished public draft"})
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex wait for cancellation"); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(2 * time.Second)
	started := false
	stopped := false
	for {
		select {
		case event := <-orchestrator.Events():
			switch event.Type {
			case EventTurnStarted:
				if event.Participant == chat.Codex {
					started = true
				}
			case EventAgent:
				if event.AgentEvent != nil && event.AgentEvent.Agent == chat.Codex && event.AgentEvent.Type == agent.EventDelta {
					stopped = true
					orchestrator.Stop()
				}
			case EventTurnFinished:
				if event.Participant == chat.Codex {
					if !started || !stopped {
						t.Fatal("turn finished before its streamed draft was stopped")
					}
					roomState, messages := orchestrator.Snapshot()
					if roomState.Sessions[chat.Codex].Cursor != 0 {
						t.Fatalf("interrupted turn advanced cursor: %+v", roomState.Sessions[chat.Codex])
					}
					found := false
					for _, message := range messages {
						found = found || (message.Author == chat.Codex && message.Kind == chat.MessageInterrupted && message.Text == "unfinished public draft")
					}
					if !found {
						t.Fatalf("interrupted draft not saved: %v", messages)
					}
					return
				}
			}
		case <-timeout:
			t.Fatal("timed out waiting for canceled turn to finish")
		}
	}
}

func TestProviderCancellationDoesNotMarkAgentAsErrored(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 1)
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	codexAgent.run = func(_ context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		emit(agent.Event{Type: agent.EventDelta, Agent: chat.Codex, Text: "partial response"})
		return agent.TurnResult{}, fmt.Errorf("provider stopped: %w", context.Canceled)
	}
	if err := orchestrator.Post("@codex start work"); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventError {
				t.Fatalf("provider cancellation was exposed as an error: %v", event.Err)
			}
			if event.Type == EventRoundDone {
				if !strings.Contains(event.Text, "canceled") {
					t.Fatalf("round result did not describe cancellation: %q", event.Text)
				}
				_, messages := orchestrator.Snapshot()
				if !slices.ContainsFunc(messages, func(message chat.Message) bool {
					return message.Author == chat.Codex && message.Kind == chat.MessageInterrupted && message.Text == "partial response"
				}) {
					t.Fatalf("interrupted draft was not retained: %+v", messages)
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for canceled turn")
		}
	}
}

func TestNaturalAccessRequestIsApprovedAndRetried(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t, 3)
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
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

func TestDoneResponsesStillReceiveOneCrossReviewWave(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	codexAgent.run = doneRun(chat.Codex)
	claudeAgent.run = doneRun(chat.Claude)
	defer orchestrator.Close()
	if err := orchestrator.Post("reach consensus"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
}

func TestCrossReviewInstructsAgentsToRemainSilentWithoutFindings(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	reviewPrompts := make(chan string, 2)
	for _, fake := range []*fakeAgent{codexAgent, claudeAgent} {
		participant := fake.participant
		fake.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if call == 2 {
				reviewPrompts <- request.SystemPrompt
				return agent.TurnResult{Done: true}, nil
			}
			return agent.TurnResult{Text: string(participant) + " answer", Done: true}, nil
		}
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("review this"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	close(reviewPrompts)
	for prompt := range reviewPrompts {
		if !strings.Contains(prompt, "remain publicly silent") || !strings.Contains(prompt, "private done:true marker") {
			t.Errorf("review prompt does not require silence: %s", prompt)
		}
	}
	_, messages := orchestrator.Snapshot()
	publicByAgent := 0
	for _, message := range messages {
		if message.Author.ValidAgent() && message.Kind == chat.MessageText {
			publicByAgent++
		}
	}
	if publicByAgent != 2 {
		t.Fatalf("silent review created public messages: %v", messages)
	}
}

func TestOneShotAskRunsExactlyOneIndependentReadOnlyWave(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	var requestsMu sync.Mutex
	var requests []agent.TurnRequest
	for _, fake := range []*fakeAgent{codexAgent, claudeAgent} {
		participant := fake.participant
		fake.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			requestsMu.Lock()
			requests = append(requests, request)
			requestsMu.Unlock()
			return agent.TurnResult{Text: string(participant) + " answer", Done: false}, nil
		}
	}
	defer orchestrator.Close()
	if err := orchestrator.Ask("what model are you running?"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 1 || claudeAgent.callCount() != 1 {
		t.Fatalf("one-shot calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	for _, request := range requests {
		if request.Settings.Permissions != chat.PermissionReadOnly || !strings.Contains(request.Prompt, "HOST-ENFORCED TURN PERMISSIONS: READ-ONLY") {
			t.Errorf("one-shot request=%+v", request)
		}
		if !strings.Contains(request.SystemPrompt, "only response wave") {
			t.Errorf("missing one-shot instruction: %s", request.SystemPrompt)
		}
	}
}

func TestClaudeReceivesConciseResponseStyle(t *testing.T) {
	orchestrator, _, claudeAgent := newTestOrchestrator(t, 3)
	requestSeen := make(chan agent.TurnRequest, 1)
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		requestSeen <- request
		return agent.TurnResult{Text: "Concise.", Done: true}, nil
	}
	defer orchestrator.Close()
	if err := orchestrator.Ask("answer briefly"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	request := <-requestSeen
	for _, expected := range []string{"Keep public replies especially concise", "Do not provide an unsolicited workspace inventory"} {
		if !strings.Contains(request.SystemPrompt, expected) {
			t.Errorf("Claude system prompt missing %q: %s", expected, request.SystemPrompt)
		}
	}
}

func TestFourPresentAgentsEachCompleteTwoConcurrentWaves(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{
		chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true,
	}
	agents := make(map[chat.Participant]*fakeAgent)
	values := make([]agent.Agent, 0, len(chat.Agents()))
	for _, participant := range chat.Agents() {
		fake := &fakeAgent{participant: participant, run: doneRun(participant)}
		agents[participant] = fake
		values = append(values, fake)
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("reach a four-agent consensus"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	for _, participant := range chat.Agents() {
		if got := agents[participant].callCount(); got != 2 {
			t.Errorf("%s calls=%d want=2", participant, got)
		}
	}
	_, messages := orchestrator.Snapshot()
	authors := make(map[chat.Participant]int)
	for _, message := range messages {
		if message.Author.ValidAgent() && message.Kind == chat.MessageText {
			authors[message.Author]++
		}
	}
	for _, participant := range chat.Agents() {
		if authors[participant] != 2 {
			t.Fatalf("authors=%v", authors)
		}
	}
	reloaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, participant := range chat.Agents() {
		if reloaded.Sessions[participant].ID != string(participant)+"-session" {
			t.Errorf("persisted %s session=%+v", participant, reloaded.Sessions[participant])
		}
	}
}

func TestAgentLeaveAndReturnRetainsSessionAndCatchesUp(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	codexAgent := &fakeAgent{participant: chat.Codex, run: doneRun(chat.Codex)}
	claudeAgent := &fakeAgent{participant: chat.Claude, run: doneRun(chat.Claude)}
	agyAgent := &fakeAgent{participant: chat.Agy, run: doneRun(chat.Agy)}
	orchestrator, err := New(roomState, nil, roomStore, codexAgent, claudeAgent, agyAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	if err := orchestrator.SetPresence(chat.Agy, true); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@agy remember this turn"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	beforeLeave, _ := orchestrator.Snapshot()
	savedSession := beforeLeave.Sessions[chat.Agy]
	if savedSession.ID != "agy-session" || savedSession.Cursor == 0 {
		t.Fatalf("initial AGY session=%+v", savedSession)
	}

	if err := orchestrator.SetPresence(chat.Agy, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@agy should not run"); err == nil || !strings.Contains(err.Error(), "away") {
		t.Fatalf("message to away AGY error=%v", err)
	}
	if err := orchestrator.Post("@codex discuss this while AGY is away"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if agyAgent.callCount() != 1 {
		t.Fatalf("away AGY calls=%d", agyAgent.callCount())
	}

	if err := orchestrator.SetPresence(chat.Agy, true); err != nil {
		t.Fatal(err)
	}
	afterReturn, _ := orchestrator.Snapshot()
	if afterReturn.Sessions[chat.Agy] != savedSession {
		t.Fatalf("return changed session: got=%+v want=%+v", afterReturn.Sessions[chat.Agy], savedSession)
	}
	promptSeen := make(chan string, 1)
	agyAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		promptSeen <- request.Prompt
		return agent.TurnResult{Text: "caught up", SessionID: "agy-session", Done: true}, nil
	}
	if err := orchestrator.Post("@agy catch up now"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	prompt := <-promptSeen
	for _, wanted := range []string{"discuss this while AGY is away", "codex done", "agy joined the room", "catch up now"} {
		if !strings.Contains(prompt, wanted) {
			t.Errorf("catch-up prompt missing %q:\n%s", wanted, prompt)
		}
	}
	reloaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Present(chat.Agy) || reloaded.Sessions[chat.Agy].ID != "agy-session" {
		t.Fatalf("persisted room=%+v", reloaded)
	}
}

func TestLegacyRoomDefaultsToCodexAndClaudeRoster(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = nil
	orchestrator, err := New(roomState, nil, roomStore,
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude}, &fakeAgent{participant: chat.Agy},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	got, _ := orchestrator.Snapshot()
	if !slices.Equal(got.PresentAgents(), chat.DefaultAgents()) || got.Present(chat.Agy) {
		t.Fatalf("legacy roster=%v", got.PresentAgents())
	}
}

func TestEmptyRosterRequiresAnAgentToJoin(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("hello empty room"); err == nil || !strings.Contains(err.Error(), "no agents") {
		t.Fatalf("post error=%v", err)
	}
	if err := orchestrator.SetPresence(chat.Codex, true); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex hello"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
}

func TestMaterialDisagreementPausesAndPersistsUntilUserActs(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t, 3)
	codexAgent.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "This approach is unsafe", SessionID: "codex-session", Done: true, Disagrees: true, ConflictReason: "unsafe migration order"}, nil
	}
	claudeAgent.run = doneRun(chat.Claude)
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
	if codexAgent.callCount() != 3 || claudeAgent.callCount() != 3 {
		t.Fatalf("capped calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	roomState, _ := orchestrator.Snapshot()
	if roomState.Conflict == nil || roomState.Conflict.RaisedBy != chat.Codex || roomState.Conflict.Wave != 3 || roomState.Conflict.Reasons[chat.Codex] != "unsafe migration order" {
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

func newTestOrchestrator(t *testing.T, maxWaves int) (*Orchestrator, *fakeAgent, *fakeAgent) {
	t.Helper()
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), maxWaves)
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
