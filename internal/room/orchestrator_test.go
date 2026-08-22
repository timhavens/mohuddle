package room

import (
	"context"
	"fmt"
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
	requests    []agent.TurnRequest
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
	f.requests = append(f.requests, request)
	call := len(f.requests)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, call, request, emit)
	}
	if request.Ephemeral {
		return bidResult(f.participant, f.participant), nil
	}
	return agent.TurnResult{Text: string(f.participant) + " done", SessionID: string(f.participant) + "-session", Done: true}, nil
}
func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}
func (f *fakeAgent) request(index int) agent.TurnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[index]
}

func bidResult(participant, preferred chat.Participant) agent.TurnResult {
	return agent.TurnResult{
		Text: fmt.Sprintf(`{"participant":%q,"preferred_lead":%q,"fit":"high","reason":"best fit"}`, participant, preferred),
		Done: true,
	}
}

func TestUntaggedMessageRunsPrivateBidsThenModerator(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	for _, fake := range []*fakeAgent{codexAgent, claudeAgent} {
		participant := fake.participant
		fake.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if request.Ephemeral {
				started <- participant
				<-release
				return bidResult(participant, participant), nil
			}
			return agent.TurnResult{Text: "moderated answer", SessionID: "codex-session", Done: true}, nil
		}
	}
	if err := orchestrator.Post("choose the best lead"); err != nil {
		t.Fatal(err)
	}
	seen := map[chat.Participant]bool{}
	for len(seen) < 2 {
		select {
		case participant := <-started:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("private bids did not overlap")
		}
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	if !codexAgent.request(0).Ephemeral || !claudeAgent.request(0).Ephemeral || codexAgent.request(1).Ephemeral {
		t.Fatal("bid and moderator execution purposes were not separated")
	}
	for _, request := range []agent.TurnRequest{codexAgent.request(0), claudeAgent.request(0)} {
		if !request.NoTools || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
			t.Fatalf("private bid was not transcript-only: %+v", request)
		}
	}
	roomState, messages := orchestrator.Snapshot()
	if roomState.Sessions[chat.Claude] != (chat.AgentSession{}) {
		t.Fatalf("private bid changed Claude session: %+v", roomState.Sessions[chat.Claude])
	}
	for _, message := range messages {
		if strings.Contains(message.Text, "preferred_lead") {
			t.Fatalf("private bid leaked into transcript: %+v", message)
		}
	}
}

func TestModeratorDelegatesAndSilentlyRetakesFloor(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	var moderatorTurns int
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Claude), nil
		}
		moderatorTurns++
		if moderatorTurns == 1 {
			return agent.TurnResult{Done: false, Next: chat.Claude}, nil
		}
		if !strings.Contains(request.Prompt, "Claude handled it") {
			t.Errorf("moderator did not receive delegated response: %s", request.Prompt)
		}
		return agent.TurnResult{Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Claude), nil
		}
		if request.Settings.Permissions != chat.PermissionWorkspace {
			t.Errorf("delegated worker permission=%s", request.Settings.Permissions)
		}
		return agent.TurnResult{Text: "Claude handled it", SessionID: "claude-session", Done: true}, nil
	}
	if err := orchestrator.Post("implement this"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 3 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	_, messages := orchestrator.Snapshot()
	public := 0
	for _, message := range messages {
		if message.Author.ValidAgent() && message.Kind == chat.MessageText {
			public++
		}
	}
	if public != 1 {
		t.Fatalf("routing or closing chatter leaked: %+v", messages)
	}
}

func TestModeratorMayInviteBothVoiceAgentsOnce(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	var moderatorTurns int
	agySeen := false
	copilotSeen := false
	agents[chat.Codex].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		moderatorTurns++
		switch moderatorTurns {
		case 1:
			return agent.TurnResult{Done: false, Next: chat.Agy}, nil
		case 2:
			return agent.TurnResult{Done: false, Next: chat.Copilot}, nil
		default:
			return agent.TurnResult{Done: true}, nil
		}
	}
	for _, participant := range []chat.Participant{chat.Agy, chat.Copilot} {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if !request.VoiceOnly || request.Settings.Permissions != chat.PermissionReadOnly || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
				t.Errorf("%s voice request=%+v", participant, request)
			}
			if !strings.Contains(request.SystemPrompt, "Never request access") || !strings.Contains(request.Prompt, "Do not suggest changing this role") {
				t.Errorf("%s missing voice contract", participant)
			}
			if participant == chat.Agy {
				agySeen = true
			} else {
				copilotSeen = true
			}
			return agent.TurnResult{Text: string(participant) + " perspective", Done: true}, nil
		}
	}
	if err := orchestrator.Post("get useful perspectives"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if !agySeen || !copilotSeen || agents[chat.Agy].callCount() != 1 || agents[chat.Copilot].callCount() != 1 {
		t.Fatalf("voice calls agy=%d copilot=%d", agents[chat.Agy].callCount(), agents[chat.Copilot].callCount())
	}
}

func TestDirectTagInvokesExactlyOneAgent(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.Post("@claude answer directly"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 0 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	if claudeAgent.request(0).Settings.Permissions != chat.PermissionWorkspace {
		t.Fatalf("direct worker permission=%s", claudeAgent.request(0).Settings.Permissions)
	}
}

func TestDirectVoiceTagIsToolFree(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.Post("@agy answer from the discussion"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if agents[chat.Agy].callCount() != 1 {
		t.Fatalf("AGY calls=%d", agents[chat.Agy].callCount())
	}
	request := agents[chat.Agy].request(0)
	if !request.VoiceOnly || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
		t.Fatalf("voice request=%+v", request)
	}
	for participant, fake := range agents {
		if participant != chat.Agy && fake.callCount() != 0 {
			t.Fatalf("direct voice turn also invoked %s", participant)
		}
	}
}

func TestAskSelectedAgentsRunsOneConcurrentTurnEach(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	for _, participant := range []chat.Participant{chat.Claude, chat.Agy} {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			<-release
			if participant == chat.Claude && request.Settings.Permissions != chat.PermissionReadOnly {
				t.Errorf("one-shot worker permission=%s", request.Settings.Permissions)
			}
			if participant == chat.Agy && !request.VoiceOnly {
				t.Error("one-shot AGY was not voice-only")
			}
			return agent.TurnResult{Text: string(participant), Done: true}, nil
		}
	}
	if err := orchestrator.Ask("@claude @agy compare these ideas"); err != nil {
		t.Fatal(err)
	}
	seen := map[chat.Participant]bool{}
	for len(seen) < 2 {
		select {
		case participant := <-started:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("selected one-shot agents did not overlap")
		}
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	for participant, fake := range agents {
		want := 0
		if participant == chat.Claude || participant == chat.Agy {
			want = 1
		}
		if fake.callCount() != want {
			t.Errorf("%s calls=%d want=%d", participant, fake.callCount(), want)
		}
	}
}

func TestModeratorPersistsAndFallsBackWhenLeaving(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	if orchestrator.Moderator() != chat.Codex {
		t.Fatalf("default moderator=%s", orchestrator.Moderator())
	}
	if err := orchestrator.SetModerator(chat.Claude); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	roomState, _ := orchestrator.Snapshot()
	if roomState.Moderator != chat.Codex {
		t.Fatalf("fallback moderator=%s", roomState.Moderator)
	}
}

func TestVoicePermissionsArePinnedReadOnly(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	value := chat.AgentSettings{Model: "voice-model", Effort: "low", Permissions: chat.PermissionFull}
	if err := orchestrator.SetAgentSettings(chat.Agy, value, false); err != nil {
		t.Fatal(err)
	}
	got := orchestrator.EffectiveSettings()[chat.Agy]
	if got.Permissions != chat.PermissionReadOnly || got.Model != "voice-model" || got.Effort != "low" {
		t.Fatalf("effective AGY settings=%+v", got)
	}
	if agents[chat.Agy].configured.Permissions != chat.PermissionReadOnly {
		t.Fatalf("adapter AGY settings=%+v", agents[chat.Agy].configured)
	}
}

func TestVoiceDemotionClearsLegacySessionsAndGrants(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members[chat.Agy] = true
	roomState.Members[chat.Copilot] = true
	roomState.Sessions[chat.Agy] = chat.AgentSession{ID: "legacy-agy", Cursor: 8}
	roomState.Sessions[chat.Copilot] = chat.AgentSession{ID: "legacy-copilot", Cursor: 9}
	roomState.Grants = append(roomState.Grants,
		chat.AccessGrant{Path: t.TempDir(), Mode: chat.AccessRead, Participant: chat.Agy},
		chat.AccessGrant{Path: t.TempDir(), Mode: chat.AccessReadWrite, Participant: chat.Copilot},
	)
	orchestrator, err := New(roomState, nil, roomStore,
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude},
		&fakeAgent{participant: chat.Agy}, &fakeAgent{participant: chat.Copilot},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	got, _ := orchestrator.Snapshot()
	if got.Sessions[chat.Agy] != (chat.AgentSession{}) || got.Sessions[chat.Copilot] != (chat.AgentSession{}) {
		t.Fatalf("legacy voice sessions survived demotion: %+v", got.Sessions)
	}
	for _, grant := range got.Grants {
		if grant.Participant.VoiceOnly() {
			t.Fatalf("legacy voice grant survived demotion: %+v", grant)
		}
	}
}

func TestEveryVoiceTurnReceivesFullTranscript(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	var prompts []string
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		prompts = append(prompts, request.Prompt)
		return agent.TurnResult{Text: "voice reply", SessionID: "must-not-affect-context", Done: true}, nil
	}
	if err := orchestrator.Post("@agy first topic"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if err := orchestrator.Post("@agy second topic"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if len(prompts) != 2 || !strings.Contains(prompts[1], "first topic") || !strings.Contains(prompts[1], "second topic") || !strings.Contains(prompts[1], "voice reply") {
		t.Fatalf("second voice prompt did not reconstruct the transcript: %q", prompts)
	}
}

func TestMarkerOnlyCompletionDoesNotCreatePlaceholder(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{SessionID: "codex-session", Done: true}, nil
	}
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
		t.Fatalf("quiet completion did not persist progress: %+v", roomState.Sessions[chat.Codex])
	}
}

func TestVoiceAccessRequestFailsClosed(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	agents[chat.Copilot].run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{AccessRequest: &agent.AccessRequest{Path: "..", Mode: chat.AccessRead}}, nil
	}
	if err := orchestrator.Post("@copilot inspect files"); err != nil {
		t.Fatal(err)
	}
	foundError := false
	waitForRoundAllowError(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventError && strings.Contains(event.Err.Error(), "voice-only") {
			foundError = true
		}
	})
	if !foundError {
		t.Fatal("voice access attempt did not surface a host contract error")
	}
	roomState, _ := orchestrator.Snapshot()
	if len(roomState.Grants) != 1 {
		t.Fatalf("voice access attempt changed grants: %+v", roomState.Grants)
	}
}

func TestNaturalAccessRequestIsApprovedAndRetried(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	extra := t.TempDir()
	codexAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			return agent.TurnResult{Text: "need context", SessionID: "codex-session", AccessRequest: &agent.AccessRequest{Path: extra, Mode: chat.AccessRead, Reason: "supporting files"}}, nil
		}
		found := false
		for _, root := range request.ReadRoots {
			found = found || root == extra
		}
		if !found {
			t.Errorf("approved root missing from retry: %v", request.ReadRoots)
		}
		return agent.TurnResult{Text: "continued", SessionID: "codex-session", Done: true}, nil
	}
	if err := orchestrator.Post("@codex use another directory"); err != nil {
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

func TestLegacyRoomGetsCodexModerator(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Moderator = ""
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	got, _ := orchestrator.Snapshot()
	if got.Moderator != chat.Codex {
		t.Fatalf("migrated moderator=%s", got.Moderator)
	}
}

func TestSettingsRequireFullAcknowledgement(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
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
		t.Fatal("full access accepted without acknowledgement")
	}
	if err := orchestrator.AcknowledgeFullAccess(); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetAgentSettings(chat.Codex, value, false); err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.EffectiveSettings()[chat.Codex]; got != value {
		t.Fatalf("effective=%+v want=%+v", got, value)
	}
}

func TestParseAskTargets(t *testing.T) {
	participants, prompt, err := parseAsk("@claude @agy compare this\ncarefully")
	if err != nil {
		t.Fatal(err)
	}
	if len(participants) != 2 || participants[0] != chat.Claude || participants[1] != chat.Agy || prompt != "compare this\ncarefully" {
		t.Fatalf("participants=%v prompt=%q", participants, prompt)
	}
	if _, _, err := parseAsk("@unknown hello"); err == nil {
		t.Fatal("unknown ask target accepted")
	}
}

func newTestOrchestrator(t *testing.T) (*Orchestrator, *fakeAgent, *fakeAgent) {
	t.Helper()
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
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

func newFourAgentOrchestrator(t *testing.T) (*Orchestrator, map[chat.Participant]*fakeAgent) {
	t.Helper()
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	agents := make(map[chat.Participant]*fakeAgent)
	values := make([]agent.Agent, 0, len(chat.Agents()))
	for _, participant := range chat.Agents() {
		fake := &fakeAgent{participant: participant}
		agents[participant] = fake
		values = append(values, fake)
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator, agents
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

func waitForRoundAllowError(t *testing.T, events <-chan Event, inspect func(Event)) {
	t.Helper()
	timeout := time.After(4 * time.Second)
	for {
		select {
		case event := <-events:
			if inspect != nil {
				inspect(event)
			}
			if event.Type == EventRoundDone {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for round")
		}
	}
}
