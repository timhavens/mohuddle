package room

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type launchTrackingAgent struct {
	participant chat.Participant
	current     chat.AgentSettings
	configured  chat.AgentSettings
}

func (a *launchTrackingAgent) Participant() chat.Participant { return a.participant }
func (a *launchTrackingAgent) Close() error                  { return nil }
func (a *launchTrackingAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Done: true}, nil
}
func (a *launchTrackingAgent) Configure(value chat.AgentSettings) bool {
	reset := a.current.Model != value.Model || a.current.Effort != value.Effort
	a.current = value
	a.configured = value
	return reset
}

type failingAppendStore struct{}

func (failingAppendStore) SaveRoom(chat.Room) error { return nil }
func (failingAppendStore) AppendMessage(string, chat.Message) error {
	return errors.New("append failed")
}

type controlledStore struct {
	base Store
	mu   sync.Mutex
	fail struct {
		append bool
		save   bool
	}
}

func (s *controlledStore) failNextAppend() {
	s.mu.Lock()
	s.fail.append = true
	s.mu.Unlock()
}

func (s *controlledStore) failNextSave() {
	s.mu.Lock()
	s.fail.save = true
	s.mu.Unlock()
}

func (s *controlledStore) SaveRoom(value chat.Room) error {
	s.mu.Lock()
	if s.fail.save {
		s.fail.save = false
		s.mu.Unlock()
		return errors.New("controlled save failure")
	}
	s.mu.Unlock()
	return s.base.SaveRoom(value)
}

func (s *controlledStore) AppendMessage(roomID string, message chat.Message) error {
	s.mu.Lock()
	if s.fail.append {
		s.fail.append = false
		s.mu.Unlock()
		return errors.New("controlled append failure")
	}
	s.mu.Unlock()
	return s.base.AppendMessage(roomID, message)
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

func TestUntaggedMessageRunsPrivateBidsThenBothCoreAgents(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			started <- chat.Codex
			<-release
			return bidResult(chat.Codex, chat.Claude), nil
		}
		if !strings.Contains(request.Prompt, "Claude lead answer") {
			t.Errorf("moderator did not receive lead response: %s", request.Prompt)
		}
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("moderator review permission=%s", request.Settings.Permissions)
		}
		return agent.TurnResult{Done: true, SessionID: "codex-session"}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			started <- chat.Claude
			<-release
			return bidResult(chat.Claude, chat.Claude), nil
		}
		if request.Settings.Permissions != chat.PermissionWorkspace {
			t.Errorf("lead permission=%s", request.Settings.Permissions)
		}
		return agent.TurnResult{Text: "Claude lead answer", SessionID: "claude-session", Done: true}, nil
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
	if codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	if !codexAgent.request(0).Ephemeral || !claudeAgent.request(0).Ephemeral || codexAgent.request(1).Ephemeral || claudeAgent.request(1).Ephemeral {
		t.Fatal("bid and public execution purposes were not separated")
	}
	for _, request := range []agent.TurnRequest{codexAgent.request(0), claudeAgent.request(0)} {
		if !request.NoTools || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
			t.Fatalf("private bid was not transcript-only: %+v", request)
		}
	}
	roomState, messages := orchestrator.Snapshot()
	if roomState.Sessions[chat.Claude].ID != "claude-session" {
		t.Fatalf("lead session was not saved: %+v", roomState.Sessions[chat.Claude])
	}
	for _, message := range messages {
		if strings.Contains(message.Text, "preferred_lead") {
			t.Fatalf("private bid leaked into transcript: %+v", message)
		}
	}
}

func TestDirectMessageCarriesImageAttachmentAndReadableRoot(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	path := filepath.Join(t.TempDir(), "screen.png")
	attachment := chat.Attachment{ID: "image", Kind: chat.AttachmentImage, Name: "screen.png", MIMEType: "image/png", Path: path}
	if err := orchestrator.PostWithAttachments("@codex inspect this", []chat.Attachment{attachment}); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 1 {
		t.Fatalf("Codex calls=%d", codexAgent.callCount())
	}
	request := codexAgent.request(0)
	if len(request.Attachments) != 1 || request.Attachments[0].Path != path || !strings.Contains(request.Prompt, "[Image #1 attached: "+path+"]") {
		t.Fatalf("request=%+v", request)
	}
	foundRoot := false
	for _, root := range request.ReadRoots {
		foundRoot = foundRoot || root == filepath.Dir(path)
	}
	if !foundRoot {
		t.Fatalf("attachment root missing from %v", request.ReadRoots)
	}
	_, messages := orchestrator.Snapshot()
	if len(messages) == 0 || len(messages[0].Attachments) != 1 || messages[0].Attachments[0].ID != "image" {
		t.Fatalf("messages=%+v", messages)
	}
}

func TestModeratorLeadReceivesPeerReviewBeforeClosing(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	var moderatorTurns int
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		moderatorTurns++
		if moderatorTurns == 1 {
			if request.Settings.Permissions != chat.PermissionWorkspace {
				t.Errorf("lead permission=%s", request.Settings.Permissions)
			}
			return agent.TurnResult{Text: "Codex lead answer", SessionID: "codex-session", Done: true}, nil
		}
		if request.Settings.Permissions != chat.PermissionReadOnly || !strings.Contains(request.Prompt, "Claude review") || !strings.Contains(request.SystemPrompt, "peer scope concern") {
			t.Errorf("moderator closing request=%+v", request)
		}
		return agent.TurnResult{Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Codex), nil
		}
		if request.Settings.Permissions != chat.PermissionReadOnly || !strings.Contains(request.Prompt, "Codex lead answer") {
			t.Errorf("peer review request=%+v", request)
		}
		return agent.TurnResult{Text: "Claude review", SessionID: "claude-session", Done: false, Disagrees: true, ConflictReason: "peer scope concern"}, nil
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
	if public != 2 {
		t.Fatalf("unexpected public messages: %+v", messages)
	}
}

func TestMissingNextAutomaticallyInvitesBothVoiceAgentsOnce(t *testing.T) {
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
			return agent.TurnResult{Text: "core lead", Done: true}, nil
		case 2:
			return agent.TurnResult{Done: false}, nil
		case 3:
			if !strings.Contains(request.Prompt, "agy perspective") {
				t.Errorf("moderator did not receive AGY response: %s", request.Prompt)
			}
			return agent.TurnResult{Done: false}, nil
		case 4:
			if !strings.Contains(request.Prompt, "copilot perspective") {
				t.Errorf("moderator did not receive Copilot response: %s", request.Prompt)
			}
			return agent.TurnResult{Done: true}, nil
		default:
			t.Fatalf("unexpected moderator turn %d", moderatorTurns)
			return agent.TurnResult{}, nil
		}
	}
	agents[chat.Claude].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Codex), nil
		}
		return agent.TurnResult{Done: true}, nil
	}
	for _, participant := range []chat.Participant{chat.Agy, chat.Copilot} {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if !request.VoiceOnly || request.Settings.Permissions != chat.PermissionReadOnly || len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
				t.Errorf("%s voice request=%+v", participant, request)
			}
			if !strings.Contains(request.SystemPrompt, "Never request access") || !strings.Contains(request.Prompt, "Do not suggest changing your permissions") {
				t.Errorf("%s missing isolated read-only contract", participant)
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
	roomState, _ := orchestrator.Snapshot()
	if roomState.Conflict != nil {
		t.Fatalf("neutral incomplete handoff created conflict: %+v", roomState.Conflict)
	}
}

func TestNeutralIncompleteModeratorEndingIsNotAConflict(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Claude), nil
		}
		return agent.TurnResult{Done: false}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Claude), nil
		}
		return agent.TurnResult{Text: "lead answer", Done: true}, nil
	}
	if err := orchestrator.Post("answer together"); err != nil {
		t.Fatal(err)
	}
	warning := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		warning = warning || event.Type == EventWarning
	})
	roomState, _ := orchestrator.Snapshot()
	if !warning || roomState.Conflict != nil {
		t.Fatalf("warning=%v conflict=%+v", warning, roomState.Conflict)
	}
}

func TestExplicitModeratorDisagreementCreatesConflict(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Claude), nil
		}
		return agent.TurnResult{Done: false, Disagrees: true, ConflictReason: "material scope mismatch"}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Claude), nil
		}
		return agent.TurnResult{Text: "lead answer", Done: true}, nil
	}
	if err := orchestrator.Post("resolve the scope"); err != nil {
		t.Fatal(err)
	}
	waitForConflict(t, orchestrator.Events())
	roomState, _ := orchestrator.Snapshot()
	if roomState.Conflict == nil || !strings.Contains(roomState.Conflict.Reason, "material scope mismatch") {
		t.Fatalf("conflict=%+v", roomState.Conflict)
	}
	resumeContextSeen := false
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Claude), nil
		}
		resumeContextSeen = resumeContextSeen || strings.Contains(request.SystemPrompt, "material scope mismatch")
		return agent.TurnResult{Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Claude), nil
		}
		resumeContextSeen = resumeContextSeen || strings.Contains(request.SystemPrompt, "material scope mismatch")
		return agent.TurnResult{Text: "resumed answer", Done: true}, nil
	}
	if err := orchestrator.Continue(); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if !resumeContextSeen {
		t.Fatal("continued round did not receive saved conflict context")
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

func TestDirectTagWithQuestionMarkInvokesExactlyOneAgent(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.Post("@claude?"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if codexAgent.callCount() != 0 || claudeAgent.callCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d", codexAgent.callCount(), claudeAgent.callCount())
	}
	_, messages := orchestrator.Snapshot()
	if len(messages) == 0 || messages[0].Target != chat.Claude || messages[0].Text != "?" {
		t.Fatalf("direct punctuation message=%+v", messages)
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
	if !request.PublicResponseRequired {
		t.Fatal("direct voice request did not require a public response")
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

func TestRoundSelectedAgentsRunSequentiallyBeforeReadOnlyModerator(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	var order []chat.Participant
	for _, participant := range chat.Agents() {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			order = append(order, participant)
			if request.Settings.Permissions != chat.PermissionReadOnly {
				t.Errorf("%s round permission=%s", participant, request.Settings.Permissions)
			}
			if participant == chat.Codex {
				if !strings.Contains(request.Prompt, "claude round view") || !strings.Contains(request.Prompt, "agy round view") {
					t.Errorf("moderator did not receive selected responses: %s", request.Prompt)
				}
				return agent.TurnResult{Text: "moderator synthesis", Done: true}, nil
			}
			return agent.TurnResult{Text: string(participant) + " round view", Done: true}, nil
		}
	}
	if err := orchestrator.Round("@claude @agy compare these views"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	want := []chat.Participant{chat.Claude, chat.Agy, chat.Codex}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("round order=%v want=%v", order, want)
	}
	if agents[chat.Copilot].callCount() != 0 {
		t.Fatalf("unselected Copilot calls=%d", agents[chat.Copilot].callCount())
	}
}

func TestRoundAllContinuesAfterParticipantFailure(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	var order []chat.Participant
	for _, participant := range chat.Agents() {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			order = append(order, participant)
			if participant == chat.Agy {
				return agent.TurnResult{}, errors.New("agy unavailable")
			}
			if participant == chat.Codex && !strings.Contains(request.SystemPrompt, "agy") {
				t.Errorf("moderator was not told about failed participant: %s", request.SystemPrompt)
			}
			return agent.TurnResult{Text: string(participant) + " response", Done: true}, nil
		}
	}
	if err := orchestrator.Round("hear from everyone"); err != nil {
		t.Fatal(err)
	}
	waitForRoundAllowError(t, orchestrator.Events(), nil)
	want := []chat.Participant{chat.Claude, chat.Agy, chat.Copilot, chat.Codex}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("round order=%v want=%v", order, want)
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

func TestOptionalParticipantPermissionsCanBeElevated(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	value := chat.AgentSettings{Model: "worker-model", Effort: "low", Permissions: chat.PermissionWorkspace}
	if err := orchestrator.SetAgentSettings(chat.Agy, value, false); err != nil {
		t.Fatal(err)
	}
	got := orchestrator.EffectiveSettings()[chat.Agy]
	if got.Permissions != chat.PermissionWorkspace || got.Model != "worker-model" || got.Effort != "low" {
		t.Fatalf("effective AGY settings=%+v", got)
	}
	if agents[chat.Agy].configured.Permissions != chat.PermissionWorkspace {
		t.Fatalf("adapter AGY settings=%+v", agents[chat.Agy].configured)
	}
}

func TestAuxiliaryLaunchFallbackSurvivesConfigureOnResume(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = true
	roomState.Sessions[worker] = chat.AgentSession{ID: "resumed-worker", Cursor: 12}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCounts(map[chat.Participant]int{chat.Codex: 1}); err != nil {
		t.Fatal(err)
	}
	want := chat.AgentSettings{Model: "gpt-worker", Effort: "high", Permissions: chat.PermissionReadOnly}
	workerAgent := &launchTrackingAgent{participant: worker, current: want}
	orchestrator, err := New(loaded, nil, roomStore,
		&fakeAgent{participant: chat.Codex}, workerAgent,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	launch := map[chat.Participant]chat.AgentSettings{
		chat.Codex: {Model: want.Model, Effort: want.Effort, Permissions: chat.PermissionFull},
	}
	if err := orchestrator.Configure(preferences, launch); err != nil {
		t.Fatal(err)
	}
	if workerAgent.configured != want {
		t.Fatalf("configured auxiliary settings=%+v want=%+v", workerAgent.configured, want)
	}
	resumed, _ := orchestrator.Snapshot()
	if got := resumed.Sessions[worker]; got.ID != "resumed-worker" || got.Cursor != 12 {
		t.Fatalf("Configure reset the auxiliary resume session: %+v", got)
	}
}

func TestUnconfiguredCanonicalWorkerSettingsAreRejected(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	value := chat.AgentSettings{Model: "unconfigured", Effort: "high", Permissions: chat.PermissionReadOnly}
	defaultBefore := preferences.Default(worker)
	for _, personalDefault := range []bool{false, true} {
		if err := orchestrator.SetAgentSettings(worker, value, personalDefault); err == nil || !strings.Contains(err.Error(), "not a configured participant") {
			t.Fatalf("SetAgentSettings(personalDefault=%t) error=%v, want configured-participant rejection", personalDefault, err)
		}
	}
	if err := orchestrator.InheritAgentSettings(worker); err == nil || !strings.Contains(err.Error(), "not a configured participant") {
		t.Fatalf("InheritAgentSettings error=%v, want configured-participant rejection", err)
	}
	roomState, _ := orchestrator.Snapshot()
	if _, ok := roomState.Settings[worker]; ok {
		t.Fatalf("rejected worker settings were persisted: %+v", roomState.Settings[worker])
	}
	if got := preferences.Default(worker); got != defaultBefore {
		t.Fatalf("rejected personal worker settings changed default: got=%+v want=%+v", got, defaultBefore)
	}
}

func TestOptionalParticipantSessionsAndGrantsSurviveReload(t *testing.T) {
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
	if got.Sessions[chat.Agy].ID != "legacy-agy" || got.Sessions[chat.Copilot].ID != "legacy-copilot" {
		t.Fatalf("optional participant sessions were discarded: %+v", got.Sessions)
	}
	found := map[chat.Participant]bool{}
	for _, grant := range got.Grants {
		if grant.Participant == chat.Agy || grant.Participant == chat.Copilot {
			found[grant.Participant] = true
		}
	}
	if !found[chat.Agy] || !found[chat.Copilot] {
		t.Fatalf("optional participant grants were discarded: %+v", got.Grants)
	}
}

func TestElevatedOptionalParticipantUsesWorkspaceAndSession(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetAgentSettings(chat.Agy, chat.AgentSettings{Permissions: chat.PermissionWorkspace}, false); err != nil {
		t.Fatal(err)
	}
	agents[chat.Agy].run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			if request.VoiceOnly || request.Settings.Permissions != chat.PermissionWorkspace {
				t.Errorf("elevated request=%+v", request)
			}
			if len(request.ReadRoots) == 0 || len(request.WriteRoots) == 0 {
				t.Errorf("elevated request has no workspace roots: %+v", request)
			}
			return agent.TurnResult{Text: "implemented", SessionID: "agy-worker-session", Done: true}, nil
		}
		if !request.VoiceOnly || request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("discussion request=%+v", request)
		}
		if len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
			t.Errorf("isolated request has workspace roots: %+v", request)
		}
		return agent.TurnResult{Text: "discussion", SessionID: "disposable-session", Done: true}, nil
	}
	if err := orchestrator.Post("@agy implement the change"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	roomState, _ := orchestrator.Snapshot()
	if roomState.Sessions[chat.Agy].ID != "agy-worker-session" || roomState.Sessions[chat.Agy].Cursor == 0 {
		t.Fatalf("elevated AGY session=%+v", roomState.Sessions[chat.Agy])
	}
	wantSession := roomState.Sessions[chat.Agy]
	if err := orchestrator.Ask("@agy discuss without tools"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	roomState, _ = orchestrator.Snapshot()
	if roomState.Sessions[chat.Agy] != wantSession {
		t.Fatalf("read-only discussion replaced elevated session: got=%+v want=%+v", roomState.Sessions[chat.Agy], wantSession)
	}
	if err := orchestrator.SetAgentSettings(chat.Agy, chat.AgentSettings{Permissions: chat.PermissionReadOnly}, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@agy return to isolated mode"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	roomState, _ = orchestrator.Snapshot()
	if roomState.Sessions[chat.Agy] != wantSession {
		t.Fatalf("isolated direct turn replaced elevated session: got=%+v want=%+v", roomState.Sessions[chat.Agy], wantSession)
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
		if event.Type == EventError && strings.Contains(event.Err.Error(), "isolated read-only") {
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

func TestElevatedOptionalParticipantCanRequestAccess(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetAgentSettings(chat.Copilot, chat.AgentSettings{Permissions: chat.PermissionWorkspace}, false); err != nil {
		t.Fatal(err)
	}
	extra := t.TempDir()
	agents[chat.Copilot].run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.VoiceOnly {
			t.Errorf("elevated Copilot request was voice-only")
		}
		if call == 1 {
			return agent.TurnResult{Text: "need context", SessionID: "copilot-session", AccessRequest: &agent.AccessRequest{Path: extra, Mode: chat.AccessRead, Reason: "supporting files"}}, nil
		}
		for _, root := range request.ReadRoots {
			if root == extra {
				return agent.TurnResult{Text: "continued", SessionID: "copilot-session", Done: true}, nil
			}
		}
		t.Errorf("approved root missing from retry: %v", request.ReadRoots)
		return agent.TurnResult{Text: "continued", SessionID: "copilot-session", Done: true}, nil
	}
	if err := orchestrator.Post("@copilot use another directory"); err != nil {
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
		found = found || (grant.Path == extra && grant.Participant == chat.Copilot && grant.Mode == chat.AccessRead)
	}
	if !found || agents[chat.Copilot].callCount() != 2 {
		t.Fatalf("grant=%v calls=%d", roomState.Grants, agents[chat.Copilot].callCount())
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
	agyValue := chat.AgentSettings{Model: "agy-custom", Effort: "high", Permissions: chat.PermissionFull}
	if err := orchestrator.SetAgentSettings(chat.Agy, agyValue, false); err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.EffectiveSettings()[chat.Agy]; got != agyValue {
		t.Fatalf("effective AGY=%+v want=%+v", got, agyValue)
	}
}

func TestEveryTurnHasExplicitIdentityDespiteMisleadingTranscript(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if _, err := orchestrator.appendMessage(chat.Codex, "", chat.MessageText, "Hi from Codex."); err != nil {
		t.Fatal(err)
	}
	through := orchestrator.latestSequence()
	for _, participant := range chat.Agents() {
		request := orchestrator.turnRequest(participant, turnSpec{through: through}, nil)
		identity := strings.ToUpper(string(participant))
		if !strings.Contains(request.SystemPrompt, "Your MoHuddle identity is "+identity) {
			t.Errorf("%s system prompt lacks identity: %q", participant, request.SystemPrompt)
		}
		if !strings.Contains(request.Prompt, "Hi from Codex.") {
			t.Errorf("%s regression prompt lacks misleading transcript: %q", participant, request.Prompt)
		}
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

func TestParseTargetAcceptsCommonLeadingPunctuation(t *testing.T) {
	tests := []struct {
		input       string
		participant chat.Participant
		text        string
	}{
		{input: "@claude?", participant: chat.Claude, text: "?"},
		{input: "@claude, please review", participant: chat.Claude, text: "please review"},
		{input: "@claude: thoughts?", participant: chat.Claude, text: "thoughts?"},
		{input: "@claude\tcheck this", participant: chat.Claude, text: "check this"},
		{input: "@claude-1 inspect", participant: chat.Participant("claude-1"), text: "inspect"},
	}
	for _, test := range tests {
		participant, text := parseTarget(test.input)
		if participant != test.participant || text != test.text {
			t.Errorf("parseTarget(%q)=(%s,%q)", test.input, participant, text)
		}
	}
	if participant, _ := parseTarget("please ask @claude"); participant != "" {
		t.Fatalf("non-leading mention targeted %s", participant)
	}
}

func TestSelectLeadUsesAgreementAndModeratorFallback(t *testing.T) {
	cores := []chat.Participant{chat.Codex, chat.Claude}
	agreed := []leadBid{
		{PreferredLead: chat.Claude, Valid: true},
		{PreferredLead: chat.Claude, Valid: true},
	}
	if got := selectLead(agreed, chat.Codex, cores); got != chat.Claude {
		t.Fatalf("agreed lead=%s", got)
	}
	split := []leadBid{
		{PreferredLead: chat.Codex, Valid: true},
		{PreferredLead: chat.Claude, Valid: true},
	}
	if got := selectLead(split, chat.Codex, cores); got != chat.Codex {
		t.Fatalf("split lead=%s", got)
	}
	oneValid := []leadBid{{PreferredLead: chat.Claude, Valid: true}, {PreferredLead: chat.Codex}}
	if got := selectLead(oneValid, chat.Codex, cores); got != chat.Claude {
		t.Fatalf("single valid lead=%s", got)
	}
}

func TestPresenceFailoverPromotesAndRestoresPreferredModerator(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetModerator(chat.Claude); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if got := fmt.Sprint(status.Active); got != "[codex agy]" {
		t.Fatalf("active after failover=%s", got)
	}
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy || status.Promotions[0].Replaces != chat.Claude || status.Promotions[0].Source != chat.CorePromotionPresence {
		t.Fatalf("promotions=%+v", status.Promotions)
	}
	if status.Moderator != chat.Agy || status.ModeratorPreference != chat.Claude {
		t.Fatalf("moderator=%s preference=%s", status.Moderator, status.ModeratorPreference)
	}
	if err := orchestrator.SetPresence(chat.Claude, true); err != nil {
		t.Fatal(err)
	}
	status = orchestrator.CoreStatus()
	if got := fmt.Sprint(status.Active); got != "[codex claude]" || len(status.Promotions) != 0 {
		t.Fatalf("restored status=%+v", status)
	}
	if status.Moderator != chat.Claude || status.ModeratorPreference != chat.Claude {
		t.Fatalf("restored moderator=%s preference=%s", status.Moderator, status.ModeratorPreference)
	}
}

func TestPersistedCoreStateIsNormalized(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	policy := chat.BuiltInCorePolicy()
	roomState.CorePolicy = &policy
	roomState.CorePromotions = []chat.CorePromotion{
		{Participant: chat.Agy, Replaces: chat.Participant("invalid"), Source: chat.CorePromotionManual},
		{Participant: chat.Copilot, Source: chat.CorePromotionSource("invalid")},
	}
	roomState.Moderator = chat.Codex
	roomState.ModeratorPreference = chat.Claude
	roomState.ModeratorExplicit = false
	values := []agent.Agent{
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude},
		&fakeAgent{participant: chat.Agy}, &fakeAgent{participant: chat.Copilot},
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	status := orchestrator.CoreStatus()
	if len(status.Promotions) != 0 || status.ModeratorPreference != "" || status.ModeratorExplicit {
		t.Fatalf("normalized status=%+v", status)
	}
	snapshot, _ := orchestrator.Snapshot()
	if snapshot.CorePolicy == nil || fmt.Sprint(snapshot.CorePolicy.Preferred) != "[codex claude]" || fmt.Sprint(snapshot.CorePolicy.Fallbacks) != "[agy copilot]" {
		t.Fatalf("canonical policy=%+v", snapshot.CorePolicy)
	}
}

func TestSelectingEffectiveModeratorMakesPreferenceExplicit(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if status := orchestrator.CoreStatus(); status.Moderator != chat.Codex || status.ModeratorExplicit {
		t.Fatalf("initial status=%+v", status)
	}
	if err := orchestrator.SetModerator(chat.Codex); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if !status.ModeratorExplicit || status.ModeratorPreference != chat.Codex {
		t.Fatalf("explicit status=%+v", status)
	}
}

func TestRefreshCoreStateEmitsRestorationNotice(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	retryAt := time.Now().Add(30 * time.Millisecond)
	if err := orchestrator.SetParticipantAvailability(chat.Claude, &chat.ParticipantAvailability{
		Reason: "brief limit", Source: "test", DetectedAt: time.Now(), RetryAt: &retryAt, Confidence: "confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case <-orchestrator.Events():
		default:
			goto drained
		}
	}
drained:
	time.Sleep(time.Until(retryAt) + 20*time.Millisecond)
	if err := orchestrator.RefreshCoreState(); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-orchestrator.Events():
		if event.Type != EventWarning || !strings.Contains(event.Text, "Preferred core roster restored") {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing restoration notice")
	}
}

func TestTwoMissingPreferredCoresPromoteFallbacksInConfiguredOrder(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Codex, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if got := fmt.Sprint(status.Active); got != "[agy copilot]" {
		t.Fatalf("active=%s promotions=%+v", got, status.Promotions)
	}
	if len(status.Promotions) != 2 || status.Promotions[0].Replaces != chat.Codex || status.Promotions[0].Participant != chat.Agy || status.Promotions[1].Replaces != chat.Claude || status.Promotions[1].Participant != chat.Copilot {
		t.Fatalf("promotions=%+v", status.Promotions)
	}
}

func TestAutomaticFallbackFailureAdvancesToNextAvailableFallback(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy {
		t.Fatalf("initial promotions=%+v", status.Promotions)
	}
	if err := orchestrator.PromoteCore(chat.Agy, chat.Claude); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetParticipantAvailability(chat.Agy, &chat.ParticipantAvailability{
		Reason: "fallback quota exhausted", Source: "test", DetectedAt: time.Now(), Confidence: "confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	status = orchestrator.CoreStatus()
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Copilot || status.Promotions[0].Replaces != chat.Claude {
		t.Fatalf("replacement promotions=%+v", status.Promotions)
	}
}

func TestBidLimitReconcilesBeforePublicDispatch(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	var claudeCalls atomic.Int32
	agents[chat.Claude].run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		claudeCalls.Add(1)
		return agent.TurnResult{}, &agent.AvailabilityError{
			Participant: chat.Claude, Reason: "session limit", Source: "test", Confidence: "confirmed",
		}
	}
	agents[chat.Codex].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Agy), nil
		}
		return agent.TurnResult{Done: true}, nil
	}
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Agy, chat.Agy), nil
		}
		return agent.TurnResult{Text: "fallback handled it", Done: true}, nil
	}
	if err := orchestrator.Post("handle a task despite the bid-time limit"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
	if got := claudeCalls.Load(); got != 1 {
		t.Fatalf("Claude calls=%d, want only the failed private bid", got)
	}
	status := orchestrator.CoreStatus()
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy {
		t.Fatalf("status=%+v", status)
	}
}

func TestMissingPresentRuntimeCannotBeInvited(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	codexAgent := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		return agent.TurnResult{Text: "only runtime", Done: false}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codexAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("do not invite the missing Claude runtime"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
}

func TestCorePromotionsAndPreferencesSurviveRoomRestart(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	values := []agent.Agent{
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude},
		&fakeAgent{participant: chat.Agy}, &fakeAgent{participant: chat.Copilot},
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	policy := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Claude, chat.Codex},
		Fallbacks: []chat.Participant{chat.Copilot, chat.Agy},
		Failover:  chat.CoreFailoverAuto, Restore: chat.CoreRestoreManual,
	}
	if err := orchestrator.SetCorePolicy(policy, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(loaded, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status := restarted.CoreStatus()
	if status.Inherited || fmt.Sprint(status.Policy.Preferred) != "[claude codex]" || fmt.Sprint(status.Policy.Fallbacks) != "[copilot agy]" || len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Copilot {
		t.Fatalf("restarted status=%+v", status)
	}
}

func TestInheritedCorePromotionSurvivesRestartBeforePreferencesConfigure(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	roomState.CorePolicy = nil
	roomState.CorePromotions = []chat.CorePromotion{{
		Participant: chat.Claude, Replaces: chat.Agy, Source: chat.CorePromotionManual,
		Reason: "manual promotion", PromotedAt: time.Now().UTC(),
	}}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Agy, chat.Copilot},
		Fallbacks: []chat.Participant{chat.Claude, chat.Codex},
		Failover:  chat.CoreFailoverAuto, Restore: chat.CoreRestoreManual,
	}
	if err := preferences.SetDefaultCorePolicy(policy); err != nil {
		t.Fatal(err)
	}
	values := []agent.Agent{
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude},
		&fakeAgent{participant: chat.Agy}, &fakeAgent{participant: chat.Copilot},
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Claude || status.Promotions[0].Replaces != chat.Agy {
		t.Fatalf("status=%+v", status)
	}
}

func TestCooldownRestartAndRestorationPreservePreferredAndFallbackSessions(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	roomState.Sessions[chat.Claude] = chat.AgentSession{ID: "preferred-session", Cursor: 73}
	roomState.Sessions[chat.Agy] = chat.AgentSession{ID: "fallback-session", Cursor: 29}
	values := []agent.Agent{
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude},
		&fakeAgent{participant: chat.Agy}, &fakeAgent{participant: chat.Copilot},
	}
	orchestrator, err := New(roomState, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(time.Hour).UTC()
	availability := chat.ParticipantAvailability{
		Reason: "provider cooldown", Source: "provider", DetectedAt: time.Now().UTC(),
		RetryAt: &retryAt, Confidence: "confirmed",
	}
	if err := orchestrator.SetParticipantAvailability(chat.Claude, &availability); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(loaded, nil, roomStore, values...)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	assertSessions := func(stage string) {
		t.Helper()
		current, _ := restarted.Snapshot()
		if got := current.Sessions[chat.Claude]; got.ID != "preferred-session" || got.Cursor != 73 {
			t.Fatalf("%s preferred session=%+v", stage, got)
		}
		if got := current.Sessions[chat.Agy]; got.ID != "fallback-session" || got.Cursor != 29 {
			t.Fatalf("%s fallback session=%+v", stage, got)
		}
	}
	status := restarted.CoreStatus()
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy || status.Promotions[0].Replaces != chat.Claude {
		t.Fatalf("restarted cooldown status=%+v", status)
	}
	assertSessions("after restart")
	expired := availability
	expired.RetryAt = timePointerForTest(time.Now().Add(-time.Minute).UTC())
	if err := restarted.SetParticipantAvailability(chat.Claude, &expired); err != nil {
		t.Fatal(err)
	}
	status = restarted.CoreStatus()
	if len(status.Promotions) != 0 || len(status.Availability) != 0 || fmt.Sprint(status.Active) != "[codex claude]" {
		t.Fatalf("restored cooldown status=%+v", status)
	}
	assertSessions("after restoration")
}

func timePointerForTest(value time.Time) *time.Time { return &value }

func TestWorkflowRoleSnapshotSurvivesRestoration(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	cores := orchestrator.CoreStatus().Active
	if err := orchestrator.SetPresence(chat.Claude, true); err != nil {
		t.Fatal(err)
	}
	request := orchestrator.turnRequest(chat.Agy, turnSpec{readOnly: true, coreParticipants: cores}, nil)
	if request.VoiceOnly || request.NoTools {
		t.Fatalf("captured promoted-core turn became isolated after restoration: %+v", request)
	}
}

func TestPromptFailoverLeavesSlotOpenUntilManualReplacement(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	policy := chat.BuiltInCorePolicy()
	policy.Failover = chat.CoreFailoverPrompt
	if err := orchestrator.SetCorePolicy(policy, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	if got := fmt.Sprint(status.Active); got != "[codex]" || len(status.Promotions) != 0 {
		t.Fatalf("prompt mode auto-promoted: %+v", status)
	}
	if err := orchestrator.PromoteCore(chat.Agy, chat.Claude); err != nil {
		t.Fatal(err)
	}
	status = orchestrator.CoreStatus()
	if got := fmt.Sprint(status.Active); got != "[codex agy]" || len(status.Promotions) != 1 || status.Promotions[0].Source != chat.CorePromotionManual {
		t.Fatalf("manual replacement status=%+v", status)
	}
}

func TestCorePolicyChangeDropsRedundantPromotionAndRejectsPreferredReplacement(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.PromoteCore(chat.Agy, chat.Claude); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PromoteCore(chat.Codex, chat.Claude); err == nil {
		t.Fatal("preferred peer was accepted as a replacement")
	}
	policy := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Codex, chat.Claude, chat.Agy},
		Fallbacks: []chat.Participant{chat.Copilot},
		Failover:  chat.CoreFailoverAuto, Restore: chat.CoreRestoreAuto,
	}
	if err := orchestrator.SetCorePolicy(policy, false); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	got := fmt.Sprint(status.Active)
	if len(status.Promotions) != 0 || got != "[codex claude agy]" {
		t.Fatalf("status=%+v", status)
	}
}

func TestPersonalCoreDefaultAppliesToInheritingRoomWithoutCreatingOverride(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	policy := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Agy, chat.Copilot},
		Fallbacks: []chat.Participant{chat.Codex, chat.Claude},
		Failover:  chat.CoreFailoverOff, Restore: chat.CoreRestoreManual,
	}
	if err := orchestrator.SetCorePolicy(policy, true); err != nil {
		t.Fatal(err)
	}
	status := orchestrator.CoreStatus()
	roomState, _ := orchestrator.Snapshot()
	if !status.Inherited || roomState.CorePolicy != nil || fmt.Sprint(status.Active) != "[agy copilot]" {
		t.Fatalf("status=%+v room policy=%+v", status, roomState.CorePolicy)
	}
	if got := preferences.DefaultCorePolicy(); fmt.Sprint(got.Preferred) != "[agy copilot]" {
		t.Fatalf("personal default=%+v", got)
	}
}

func TestExpiredCooldownRestoresOnlyAfterActiveWorkflowBoundary(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	retryAt := time.Now().Add(80 * time.Millisecond)
	if err := orchestrator.SetParticipantAvailability(chat.Claude, &chat.ParticipantAvailability{
		Reason: "short test cooldown", Source: "test", DetectedAt: time.Now(), RetryAt: &retryAt, Confidence: "confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agents[chat.Codex].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		started <- struct{}{}
		<-release
		return agent.TurnResult{Text: "done", Done: true}, nil
	}
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Agy, chat.Codex), nil
		}
		return agent.TurnResult{Done: true}, nil
	}
	if err := orchestrator.Post("hold the workflow across cooldown expiry"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("lead did not start")
	}
	time.Sleep(time.Until(retryAt) + 30*time.Millisecond)
	if err := orchestrator.RefreshCoreState(); err != nil {
		t.Fatal(err)
	}
	if status := orchestrator.CoreStatus(); len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy {
		t.Fatalf("promotion changed mid-workflow: %+v", status)
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
	status := orchestrator.CoreStatus()
	if len(status.Promotions) != 0 || len(status.Availability) != 0 || fmt.Sprint(status.Active) != "[codex claude]" {
		t.Fatalf("status after safe-boundary restoration=%+v", status)
	}
}

func TestPromotedReadOnlyFallbackUsesPersistentCoreSession(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatal(err)
	}
	agents[chat.Codex].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Agy), nil
		}
		return agent.TurnResult{Done: true, SessionID: "codex-session"}, nil
	}
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Agy, chat.Agy), nil
		}
		if request.VoiceOnly || request.NoTools || request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("promoted AGY request=%+v", request)
		}
		return agent.TurnResult{Text: "AGY lead", Done: true, SessionID: "agy-core-session"}, nil
	}
	if err := orchestrator.Post("let the best core peer handle this"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	roomState, _ := orchestrator.Snapshot()
	if roomState.Sessions[chat.Agy].ID != "agy-core-session" {
		t.Fatalf("promoted AGY session=%+v", roomState.Sessions[chat.Agy])
	}
}

func TestThreeCorePeersBidAndReviewWithModeratorLast(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	policy := chat.CorePolicy{
		Preferred: []chat.Participant{chat.Codex, chat.Claude, chat.Agy},
		Fallbacks: []chat.Participant{chat.Copilot}, Failover: chat.CoreFailoverAuto, Restore: chat.CoreRestoreAuto,
	}
	if err := orchestrator.SetCorePolicy(policy, false); err != nil {
		t.Fatal(err)
	}
	var orderMu sync.Mutex
	var order []chat.Participant
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude, chat.Agy} {
		participant := participant
		agents[participant].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if request.Ephemeral {
				return bidResult(participant, chat.Agy), nil
			}
			orderMu.Lock()
			order = append(order, participant)
			orderMu.Unlock()
			return agent.TurnResult{Text: string(participant), Done: true, SessionID: string(participant) + "-session"}, nil
		}
	}
	if err := orchestrator.Post("three peers should handle this in order"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if got := fmt.Sprint(order); got != "[agy claude codex]" {
		t.Fatalf("public core order=%s", got)
	}
}

func TestConfirmedSessionLimitTriggersFailoverButGenericErrorDoesNot(t *testing.T) {
	t.Run("confirmed limit", func(t *testing.T) {
		orchestrator, agents := newFourAgentOrchestrator(t)
		defer orchestrator.Close()
		agents[chat.Claude].run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{}, errors.New("You've hit your session limit · resets 1:20am (America/Port-au-Prince)")
		}
		if err := orchestrator.Post("@claude test availability"); err != nil {
			t.Fatal(err)
		}
		waitForRoundAllowError(t, orchestrator.Events(), nil)
		orchestrator.wg.Wait()
		status := orchestrator.CoreStatus()
		if _, ok := status.Availability[chat.Claude]; !ok || len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy {
			t.Fatalf("status=%+v", status)
		}
		if status.Availability[chat.Claude].RetryAt == nil {
			t.Fatal("session-limit retry time was not parsed")
		}
	})
	t.Run("ordinary failure", func(t *testing.T) {
		orchestrator, agents := newFourAgentOrchestrator(t)
		defer orchestrator.Close()
		agents[chat.Claude].run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{}, errors.New("malformed provider response")
		}
		if err := orchestrator.Post("@claude test ordinary error"); err != nil {
			t.Fatal(err)
		}
		waitForRoundAllowError(t, orchestrator.Events(), nil)
		orchestrator.wg.Wait()
		status := orchestrator.CoreStatus()
		if _, ok := status.Availability[chat.Claude]; ok || len(status.Promotions) != 0 {
			t.Fatalf("ordinary failure changed availability: %+v", status)
		}
	})
	t.Run("ambiguous reset timezone", func(t *testing.T) {
		orchestrator, agents := newFourAgentOrchestrator(t)
		defer orchestrator.Close()
		agents[chat.Claude].run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
			return agent.TurnResult{}, errors.New("You've hit your session limit · resets 1:20am (ET)")
		}
		if err := orchestrator.Post("@claude test ambiguous availability"); err != nil {
			t.Fatal(err)
		}
		warning := false
		waitForRoundAllowError(t, orchestrator.Events(), func(event Event) {
			if event.Type == EventWarning && strings.Contains(event.Text, "could not be verified") && strings.Contains(event.Text, "/core unavailable @claude until RFC3339") {
				warning = true
			}
		})
		orchestrator.wg.Wait()
		if !warning {
			t.Fatal("ambiguous reset time did not produce confirmation guidance")
		}
		if _, ok := orchestrator.CoreStatus().Availability[chat.Claude]; ok {
			t.Fatal("ambiguous timezone was classified as confirmed availability")
		}
	})
}

func TestConfirmedAvailabilityPersistsWhenFailoverDoesNotPromote(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members[chat.Agy] = true
	agyAgent := &fakeAgent{participant: chat.Agy, run: func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{}, &agent.AvailabilityError{
			Participant: chat.Agy, Reason: "quota exhausted", Source: "test", Confidence: "confirmed",
		}
	}}
	orchestrator, err := New(roomState, nil, roomStore,
		&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude}, agyAgent)
	if err != nil {
		t.Fatal(err)
	}
	policy := chat.BuiltInCorePolicy()
	policy.Failover = chat.CoreFailoverPrompt
	if err := orchestrator.SetCorePolicy(policy, false); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@agy detect and persist availability"); err != nil {
		t.Fatal(err)
	}
	waitForRoundAllowError(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	availability, ok := loaded.Availability[chat.Agy]
	if !ok || availability.Reason != "quota exhausted" || len(loaded.CorePromotions) != 0 {
		t.Fatalf("persisted room=%+v", loaded)
	}
}

func TestRepeatedConfirmedAvailabilityFailureRefreshesProviderStateButNotManualOverride(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	firstRetry := time.Now().Add(time.Hour).UTC()
	orchestrator.recordProviderAvailability(chat.Claude, &agent.AvailabilityError{Participant: chat.Claude, Reason: "first quota", Source: "provider", RetryAt: &firstRetry, Confidence: "confirmed"})
	first := orchestrator.CoreStatus().Availability[chat.Claude]
	secondRetry := time.Now().Add(2 * time.Hour).UTC()
	orchestrator.recordProviderAvailability(chat.Claude, &agent.AvailabilityError{Participant: chat.Claude, Reason: "second quota", Source: "provider", RetryAt: &secondRetry, Confidence: "confirmed"})
	second := orchestrator.CoreStatus().Availability[chat.Claude]
	if second.Reason != "second quota" || second.RetryAt == nil || !second.RetryAt.Equal(secondRetry) || second.DetectedAt.Before(first.DetectedAt) {
		t.Fatalf("repeated provider availability did not refresh state: first=%+v second=%+v", first, second)
	}
	manualRetry := time.Now().Add(3 * time.Hour).UTC()
	if err := orchestrator.SetParticipantAvailability(chat.Claude, &chat.ParticipantAvailability{Reason: "manual hold", Source: "manual", DetectedAt: time.Now().UTC(), RetryAt: &manualRetry, Confidence: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	orchestrator.recordProviderAvailability(chat.Claude, &agent.AvailabilityError{Participant: chat.Claude, Reason: "third quota", Source: "provider", RetryAt: &secondRetry, Confidence: "confirmed"})
	manual := orchestrator.CoreStatus().Availability[chat.Claude]
	if manual.Reason != "manual hold" || manual.RetryAt == nil || !manual.RetryAt.Equal(manualRetry) {
		t.Fatalf("provider failure overwrote manual availability=%+v", manual)
	}
}

func TestUnavailableRuntimeCanLeaveButCannotJoin(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetPresence(chat.Claude, false); err != nil {
		t.Fatalf("leave unavailable runtime: %v", err)
	}
	if err := orchestrator.SetPresence(chat.Claude, true); err == nil {
		t.Fatal("joining unavailable runtime succeeded")
	}
}

func TestScheduledRosterActionPersistsExecutesAndPreservesSession(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("111111111111111111111111", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	roomState.Sessions[worker] = chat.AgentSession{ID: "worker-session", Cursor: 41}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	record, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(200*time.Millisecond), "quota retry")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.RosterActions) != 1 || loaded.RosterActions[0].ID != record.ID || loaded.RosterActions[0].AuthorizedBy != chat.User || loaded.RosterActions[0].Status != chat.RosterActionPending {
		t.Fatalf("persisted scheduled action=%+v", loaded.RosterActions)
	}
	orchestrator.Close()
	orchestrator, err = New(loaded, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	waitForCondition(t, time.Second, func() bool {
		current, messages := orchestrator.Snapshot()
		if !current.Present(worker) || len(current.RosterActions) != 1 || current.RosterActions[0].Status != chat.RosterActionExecuted {
			return false
		}
		for _, message := range messages {
			if message.Author == chat.System && message.Kind == chat.MessageStatus && strings.Contains(message.Text, record.ID) && strings.Contains(message.Text, "joined the room") {
				return true
			}
		}
		return false
	})
	current, messages := orchestrator.Snapshot()
	if session := current.Sessions[worker]; session.ID != "worker-session" || session.Cursor != 41 {
		t.Fatalf("scheduled join changed worker session=%+v", session)
	}
	foundAudit := false
	for _, message := range messages {
		if message.Author == chat.System && message.Kind == chat.MessageStatus && strings.Contains(message.Text, record.ID) && strings.Contains(message.Text, "joined the room") {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("scheduled execution audit missing from messages=%+v", messages)
	}
}

func TestScheduledRosterActionCancellationPersistsAndNeverExecutes(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("222222222222222222222222", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	record, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(80*time.Millisecond), "cancel me")
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.CancelRosterAction(record.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	current, _ := orchestrator.Snapshot()
	if current.Present(worker) || len(current.RosterActions) != 1 || current.RosterActions[0].Status != chat.RosterActionCancelled || current.RosterActions[0].CompletedAt == nil {
		t.Fatalf("room after cancelled scheduled action=%+v", current)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.RosterActions) != 1 || loaded.RosterActions[0].Status != chat.RosterActionCancelled {
		t.Fatalf("persisted cancellation=%+v", loaded.RosterActions)
	}
}

func TestScheduledRosterActionWaitsForCooldownAndActiveWorkflowBoundary(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("333333333333333333333333", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	retryAt := time.Now().Add(70 * time.Millisecond).UTC()
	roomState.Availability = make(map[chat.Participant]chat.ParticipantAvailability)
	roomState.Availability[worker] = chat.ParticipantAvailability{Reason: "quota", Source: "test", DetectedAt: time.Now().UTC(), RetryAt: &retryAt, Confidence: "confirmed"}
	started := make(chan struct{})
	release := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex}
	codex.run = func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		close(started)
		select {
		case <-release:
			return agent.TurnResult{Text: "done", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}
	orchestrator, err := New(roomState, nil, roomStore, codex, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if _, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(25*time.Millisecond), "after retry"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex hold the workflow"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("direct workflow did not start")
	}
	time.Sleep(100 * time.Millisecond)
	current, _ := orchestrator.Snapshot()
	if current.Present(worker) || current.RosterActions[0].Status != chat.RosterActionPending {
		t.Fatalf("scheduled action ran during active workflow: %+v", current.RosterActions)
	}
	close(release)
	waitForCondition(t, time.Second, func() bool {
		current, _ := orchestrator.Snapshot()
		return current.Present(worker) && current.RosterActions[0].Status == chat.RosterActionExecuted
	})
}

func TestScheduledRosterActionAllowsConfiguredPrimaryAndRejectsDuplicatePending(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("444444444444444444444444", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	primary, err := orchestrator.ScheduleRosterAction(chat.RosterActionLeave, chat.Claude, time.Now().Add(time.Hour), "authorized primary leave")
	if err != nil {
		t.Fatalf("scheduled configured primary leave: %v", err)
	}
	if err := orchestrator.CancelRosterAction(primary.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(time.Hour), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(2*time.Hour), "duplicate"); err == nil {
		t.Fatal("duplicate pending worker action was accepted")
	}
	if _, err := orchestrator.ScheduleRosterAction(chat.RosterActionLeave, chat.Codex, time.Now().Add(time.Hour), strings.Repeat("x", maxRosterActionReasonBytes+1)); err == nil {
		t.Fatal("oversized scheduled roster reason was accepted")
	}
}

func TestScheduledRosterActionWithoutUserAuthorizationFailsClosed(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("555555555555555555555555", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	roomState.RosterActions = []chat.ScheduledRosterAction{{
		ID: "model-proposed", Action: chat.RosterActionJoin, Participant: worker,
		ExecuteAt: time.Now().Add(-time.Minute), CreatedAt: time.Now().Add(-time.Hour),
		AuthorizedBy: chat.Codex, Status: chat.RosterActionPending,
	}}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	waitForCondition(t, time.Second, func() bool {
		current, _ := orchestrator.Snapshot()
		return len(current.RosterActions) == 1 && current.RosterActions[0].Status == chat.RosterActionFailed
	})
	current, _ := orchestrator.Snapshot()
	if current.Present(worker) || current.RosterActions[0].Detail != "missing explicit user authorization" {
		t.Fatalf("unauthorized roster action did not fail closed: %+v", current.RosterActions[0])
	}
}

func TestScheduleRosterActionSaveFailureRollsBack(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controlled := &controlledStore{base: base}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState := chat.NewRoom("666666666666666666666666", t.TempDir(), 3, time.Now())
	roomState.Members[worker] = false
	if err := base.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, controlled, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	controlled.failNextSave()
	if _, err := orchestrator.ScheduleRosterAction(chat.RosterActionJoin, worker, time.Now().Add(time.Hour), "rollback"); err == nil {
		t.Fatal("controlled save failure was not returned")
	}
	if actions := orchestrator.RosterActions(); len(actions) != 0 {
		t.Fatalf("failed schedule mutated in-memory audit=%+v", actions)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func TestCorrectionLifecycleIsValidatedCountedAndPersisted(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "The timeout is one millisecond."}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	correction, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "It is one second, not one millisecond.", Corrects: claim}, claim, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "I accept my own correction.", Accepts: correction}, correction, false); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Accepts: correction}, correction, false); err != nil {
		t.Fatal(err)
	}
	disputedAt, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "I dispute that correction.", Disputes: correction}, correction, false)
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "I checked it and accept the correction.", Accepts: correction}, disputedAt, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "I now retract it.", Retracts: correction}, acceptedAt, false); err != nil {
		t.Fatal(err)
	}

	secondClaim, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "The retry is always safe."}, acceptedAt, false)
	if err != nil {
		t.Fatal(err)
	}
	secondCorrection, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "That retry can duplicate writes.", Corrects: secondClaim}, secondClaim, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "I withdraw that correction.", Retracts: secondCorrection}, secondCorrection, false); err != nil {
		t.Fatal(err)
	}

	thirdClaim, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "The buffer is unbounded."}, secondCorrection, false)
	if err != nil {
		t.Fatal(err)
	}
	thirdCorrection, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "The buffer is capped.", Corrects: thirdClaim}, thirdClaim, false)
	if err != nil {
		t.Fatal(err)
	}
	_, messages := orchestrator.Snapshot()
	ledger := chat.CorrectionLedger(messages)
	if len(ledger) != 3 || ledger[0].Status != chat.CorrectionAcceptedStatus || ledger[0].StatusSequence != acceptedAt || ledger[1].Status != chat.CorrectionRetractedStatus || ledger[2].Status != chat.CorrectionPendingStatus {
		t.Fatalf("ledger=%+v", ledger)
	}
	for index := range messages {
		if messages[index].Sequence == correction {
			messages[index].CorrectionEvents[0].Target = chat.Agy
		}
	}
	_, messages = orchestrator.Snapshot()
	if ledger = chat.CorrectionLedger(messages); len(ledger) != 3 || ledger[0].Target != chat.Claude {
		t.Fatalf("snapshot mutation changed transcript ledger: %+v", ledger)
	}
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}
	loadedRoom, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedMessages, err := roomStore.LoadMessages(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(loadedRoom, loadedMessages, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.recordResult(chat.Claude, agent.TurnResult{Text: "I accept the buffer correction.", Accepts: thirdCorrection}, loadedMessages[len(loadedMessages)-1].Sequence, false); err != nil {
		t.Fatal(err)
	}
	_, restartedMessages := restarted.Snapshot()
	total, agents := chat.CorrectionStatistics(restartedMessages)
	if total.Offered != 3 || total.Accepted != 2 || total.Retracted != 1 || total.Pending != 0 {
		t.Fatalf("room correction counts=%+v", total)
	}
	if agents[chat.Codex].Accepted != 2 || agents[chat.Claude].AcceptedReceived != 2 || agents[chat.Claude].Retracted != 1 {
		t.Fatalf("agent correction counts=%+v", agents)
	}
}

func TestInvalidCorrectionReferencesNeverCreateEvents(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	claim, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "A claim."}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "A self-correction.", Corrects: claim}, claim, false); err != nil {
		t.Fatal(err)
	}
	userMessage, err := orchestrator.appendMessage(chat.User, "", chat.MessageText, "A human claim.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "Trying to correct the user.", Corrects: userMessage.Sequence}, userMessage.Sequence, false); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "A future reference.", Corrects: 9999}, userMessage.Sequence, false); err != nil {
		t.Fatal(err)
	}
	_, messages := orchestrator.Snapshot()
	if ledger := chat.CorrectionLedger(messages); len(ledger) != 0 {
		t.Fatalf("invalid corrections were recorded: %+v", ledger)
	}
}

func TestCorrectionContextExposesOnlyRelevantOpenEvents(t *testing.T) {
	corrections := []chat.Correction{
		{CorrectionSequence: 12, CorrectedSequence: 10, Proposer: chat.Codex, Target: chat.Claude, Status: chat.CorrectionPendingStatus},
		{CorrectionSequence: 14, CorrectedSequence: 13, Proposer: chat.Claude, Target: chat.Agy, Status: chat.CorrectionDisputedStatus},
		{CorrectionSequence: 16, CorrectedSequence: 15, Proposer: chat.Copilot, Target: chat.Claude, Status: chat.CorrectionAcceptedStatus},
		{CorrectionSequence: 20, CorrectedSequence: 19, Proposer: chat.Codex, Target: chat.Claude, Status: chat.CorrectionPendingStatus},
	}
	claude := correctionContextFor(chat.Claude, corrections[:3])
	for _, expected := range []string{`Correction message [12] from @codex`, `"accepts":12`, `Your correction message [14] to @agy`, `"retracts":14`} {
		if !strings.Contains(claude, expected) {
			t.Fatalf("Claude context missing %q: %s", expected, claude)
		}
	}
	for _, unexpected := range []string{"[16]", "[20]"} {
		if strings.Contains(claude, unexpected) {
			t.Fatalf("Claude context exposed %q: %s", unexpected, claude)
		}
	}
	if got := correctionContextFor(chat.Copilot, corrections[:3]); got != "" {
		t.Fatalf("irrelevant context=%q", got)
	}
}

func TestCorrectionReferencesCannotExceedSuppliedTranscript(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	claim, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "A claim."}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	correction, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "A correction.", Corrects: claim}, claim, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "A conflicting resolution.", Accepts: correction, Disputes: correction}, correction, false); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "I guessed a future correction.", Accepts: correction}, claim, false); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "I guessed a future message.", Corrects: correction + 1}, correction, false); err != nil {
		t.Fatal(err)
	}
	_, messages := orchestrator.Snapshot()
	ledger := chat.CorrectionLedger(messages)
	if len(ledger) != 1 || ledger[0].Status != chat.CorrectionPendingStatus {
		t.Fatalf("invisible reference changed ledger: %+v", ledger)
	}
}

func TestConcurrentTerminalCorrectionEventsResolveByTranscriptOrder(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	claim, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "A claim."}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	correction, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "A correction.", Corrects: claim}, claim, false)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		participant chat.Participant
		sequence    uint64
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	go func() {
		<-start
		sequence, err := orchestrator.recordResult(chat.Claude, agent.TurnResult{Text: "I accept it.", Accepts: correction}, correction, false)
		results <- result{participant: chat.Claude, sequence: sequence, err: err}
	}()
	go func() {
		<-start
		sequence, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "I retract it.", Retracts: correction}, correction, false)
		results <- result{participant: chat.Codex, sequence: sequence, err: err}
	}()
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent results: %+v %+v", first, second)
	}
	earlier := first
	if second.sequence < first.sequence {
		earlier = second
	}
	_, messages := orchestrator.Snapshot()
	ledger := chat.CorrectionLedger(messages)
	if len(ledger) != 1 || ledger[0].StatusSequence != earlier.sequence {
		t.Fatalf("ledger=%+v earlier=%+v", ledger, earlier)
	}
	want := chat.CorrectionRetractedStatus
	if earlier.participant == chat.Claude {
		want = chat.CorrectionAcceptedStatus
	}
	if ledger[0].Status != want {
		t.Fatalf("status=%s want %s; events=%+v", ledger[0].Status, want, messages)
	}
}

func TestCorrectionEventIsNotCommittedWhenPublicMessageAppendFails(t *testing.T) {
	now := time.Now().UTC()
	roomState := chat.NewRoom("room", t.TempDir(), 3, now)
	claim := chat.Message{ID: "claim", Sequence: 1, Author: chat.Claude, Kind: chat.MessageText, Text: "A claim.", CreatedAt: now}
	orchestrator, err := New(roomState, []chat.Message{claim}, failingAppendStore{}, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "A correction.", Corrects: claim.Sequence}, claim.Sequence, false); err == nil {
		t.Fatal("correction append unexpectedly succeeded")
	}
	_, messages := orchestrator.Snapshot()
	if len(messages) != 1 || len(messages[0].CorrectionEvents) != 0 || len(chat.CorrectionLedger(messages)) != 0 {
		t.Fatalf("failed append changed transcript: %+v", messages)
	}
}

func TestEventSubscribersReceiveIndependentCopies(t *testing.T) {
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()
	orchestrator := &Orchestrator{
		events: make(chan Event, 2), subscribers: make(map[uint64]*eventSubscriber),
		lifetime: ctx,
	}
	stream, cancel := orchestrator.SubscribeEvents(2)
	value := Event{Type: EventWarning, Text: "test"}
	orchestrator.send(value)
	if got := <-orchestrator.events; got.Text != value.Text {
		t.Fatalf("primary event=%+v", got)
	}
	if got := <-stream; got.Text != value.Text {
		t.Fatalf("subscriber event=%+v", got)
	}
	cancel()
	if _, ok := <-stream; ok {
		t.Fatal("subscriber stream remained open after cancellation")
	}
}

func TestSlowEventSubscriberDoesNotBlockAndReceivesGapWarning(t *testing.T) {
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()
	orchestrator := &Orchestrator{
		events: make(chan Event, 8), subscribers: make(map[uint64]*eventSubscriber),
		lifetime: ctx,
	}
	stream, cancel := orchestrator.SubscribeEvents(1)
	defer cancel()
	orchestrator.send(Event{Type: EventWarning, Text: "one"})
	orchestrator.send(Event{Type: EventWarning, Text: "two"})
	if got := <-stream; got.Text != "one" {
		t.Fatalf("first event=%+v", got)
	}
	orchestrator.send(Event{Type: EventWarning, Text: "three"})
	select {
	case got := <-stream:
		if !strings.Contains(got.Text, "event stream gap") || !strings.Contains(got.Text, "reload room history") {
			t.Fatalf("gap event=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscriber gap warning")
	}
}

func TestCloneMessagesDeepCopiesRouteMetadata(t *testing.T) {
	source := []chat.Message{{Route: &chat.RouteMetadata{
		MessageID: "message", OriginInstanceID: "origin", OriginClientID: "client", Hops: []string{"origin"},
	}}}
	cloned := cloneMessages(source)
	cloned[0].Route.OriginClientID = "changed"
	cloned[0].Route.Hops[0] = "changed"
	if source[0].Route.OriginClientID != "client" || source[0].Route.Hops[0] != "origin" {
		t.Fatalf("source route changed through clone: %+v", source[0].Route)
	}
}

func TestPostExternalPersistsAuthenticatedRoute(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	route := chat.RouteMetadata{
		MessageID: "external-message", OriginInstanceID: "origin", OriginClientID: "origin/client", Hops: []string{"origin", "host"},
	}
	if err := orchestrator.PostExternal("@codex hello", route); err != nil {
		t.Fatal(err)
	}
	_, messages := orchestrator.Snapshot()
	if len(messages) == 0 || messages[0].Route == nil || messages[0].Route.MessageID != route.MessageID || len(messages[0].Route.Hops) != 2 {
		t.Fatalf("messages=%+v", messages)
	}
	messages[0].Route.Hops[0] = "changed"
	_, messages = orchestrator.Snapshot()
	if messages[0].Route.Hops[0] != "origin" {
		t.Fatalf("persisted route mutated through snapshot: %+v", messages[0].Route)
	}
	if err := orchestrator.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHumanDelegationsOverlapMainWorkflowAndRemainReadOnly(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	codexOne, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	claudeOne, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, codexOne: true, claudeOne: true}
	started := make(chan chat.Participant, 3)
	release := make(chan struct{})
	makeBlocking := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			if participant.IsAuxiliary() {
				if request.VoiceOnly || request.NoTools || request.Settings.Permissions != chat.PermissionReadOnly || len(request.ReadRoots) == 0 || len(request.WriteRoots) != 0 {
					t.Errorf("delegated request for %s was not repository-readable and write-denied: %+v", participant, request)
				}
				if !strings.Contains(request.SystemPrompt, "Do not delegate or route another participant") {
					t.Errorf("delegated task contract missing for %s", participant)
				}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return agent.TurnResult{}, ctx.Err()
			}
			return agent.TurnResult{Text: string(participant) + " result", SessionID: string(participant) + "-session", Done: true}, nil
		}}
	}
	orchestrator, err := New(roomState, nil, roomStore, makeBlocking(chat.Codex), makeBlocking(codexOne), makeBlocking(claudeOne))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex own the main task"); err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != chat.Codex {
		t.Fatalf("first started=%s", got)
	}
	if err := orchestrator.Delegate(codexOne, "inspect parsing"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Delegate(claudeOne, "inspect persistence"); err != nil {
		t.Fatal(err)
	}
	seen := map[chat.Participant]bool{}
	for len(seen) < 2 {
		select {
		case participant := <-started:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("delegated workers did not overlap the active main turn")
		}
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for orchestrator.HasActiveWork() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if orchestrator.HasActiveWork() {
		t.Fatal("delegated work did not settle")
	}
	roomCopy, messages := orchestrator.Snapshot()
	if roomCopy.Sessions[codexOne].ID != "codex-1-session" || roomCopy.Sessions[claudeOne].ID != "claude-1-session" {
		t.Fatalf("independent worker sessions were not persisted: %+v", roomCopy.Sessions)
	}
	for _, participant := range []chat.Participant{codexOne, claudeOne} {
		found := false
		for _, message := range messages {
			found = found || message.Author == participant
		}
		if !found {
			t.Fatalf("no public result attributed to %s: %+v", participant, messages)
		}
	}
}

func TestColdDelegatedWorkerReceivesOnlyBoundedHandoffContext(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, worker: true}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	orchestrator.mu.Lock()
	orchestrator.messages = nil
	for sequence := uint64(1); sequence <= 2000; sequence++ {
		orchestrator.messages = append(orchestrator.messages, chat.Message{
			Sequence: sequence, Author: chat.Codex, Kind: chat.MessageTool,
			Text: "historical-tool-record-should-not-replay " + strings.Repeat("x", 512),
		})
	}
	for sequence := uint64(2001); sequence <= 2300; sequence++ {
		orchestrator.messages = append(orchestrator.messages, chat.Message{
			Sequence: sequence, Author: chat.Codex, Kind: chat.MessageTool,
			Text: fmt.Sprintf("fresh-tool-record-%d %s", sequence, strings.Repeat("y", 512)),
		})
	}
	orchestrator.nextSequence = 2301
	orchestrator.mu.Unlock()

	request := orchestrator.turnRequest(worker, turnSpec{
		after: 2000, through: 2300, delegated: true, readOnly: true,
		coreParticipants: []chat.Participant{chat.Codex},
		instruction:      "Inspect the current handoff only.",
	}, nil)
	if strings.Contains(request.Prompt, "historical-tool-record-should-not-replay") {
		t.Fatal("cold delegated worker received pre-handoff room history")
	}
	if strings.Contains(request.Prompt, "fresh-tool-record-2001") {
		t.Fatal("delegated transcript record cap did not discard oldest handoff records")
	}
	if !strings.Contains(request.Prompt, "fresh-tool-record-2300") {
		t.Fatal("delegated transcript cap discarded the newest handoff record")
	}
	if !strings.Contains(request.Prompt, "HOST-ENFORCED CURRENT WORKFLOW INSTRUCTION:\nInspect the current handoff only.") {
		t.Fatal("current workflow instruction was not carried outside the untrusted transcript")
	}
	start := strings.Index(request.Prompt, "BEGIN UNTRUSTED ROOM TRANSCRIPT")
	if start < 0 || len(request.Prompt[start:]) > maxDelegatedTranscriptBytes {
		t.Fatalf("delegated transcript length=%d, limit=%d", len(request.Prompt[start:]), maxDelegatedTranscriptBytes)
	}
}

func TestModeratorJoinsDelegatesConcurrentlySynthesizesAndLeavesWorkers(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	codexOne, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	claudeOne, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, codexOne: false, claudeOne: false}
	workerStarted := make(chan chat.Participant, 2)
	release := make(chan struct{})
	workers := map[chat.Participant]*fakeAgent{}
	for _, participant := range []chat.Participant{codexOne, claudeOne} {
		participant := participant
		workers[participant] = &fakeAgent{participant: participant, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			workerStarted <- participant
			<-release
			return agent.TurnResult{Text: string(participant) + " delegated finding", Done: true}, nil
		}}
	}
	moderatorTurns := 0
	codex := &fakeAgent{participant: chat.Codex}
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		moderatorTurns++
		if moderatorTurns == 1 {
			return agent.TurnResult{Text: "delegation plan", Done: false,
				Joins: []chat.Participant{codexOne, claudeOne},
				Delegates: []agent.DelegationRequest{
					{Participant: codexOne, Task: "audit parsing"},
					{Participant: claudeOne, Task: "audit persistence"},
				}}, nil
		}
		if !strings.Contains(request.Prompt, "codex-1 delegated finding") || !strings.Contains(request.Prompt, "claude-1 delegated finding") {
			t.Errorf("moderator did not receive both delegated results: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "moderator synthesis", Done: true, Leaves: []chat.Participant{codexOne, claudeOne}}, nil
	}
	orchestrator, err := New(roomState, nil, roomStore, codex, workers[codexOne], workers[claudeOne])
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("use helpers"); err != nil {
		t.Fatal(err)
	}
	seen := map[chat.Participant]bool{}
	for len(seen) < 2 {
		select {
		case participant := <-workerStarted:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("moderator delegation batch did not overlap")
		}
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	if moderatorTurns != 2 {
		t.Fatalf("moderator turns=%d, want planning plus post-delegation synthesis", moderatorTurns)
	}
	roomCopy, _ := orchestrator.Snapshot()
	if roomCopy.Present(codexOne) || roomCopy.Present(claudeOne) {
		t.Fatalf("moderator leave requests were not applied: %+v", roomCopy.Members)
	}
}

func TestModeratorDelegationBatchRejectsInvalidTargetAtomically(t *testing.T) {
	orchestrator, codex, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		return agent.TurnResult{Done: true, Delegates: []agent.DelegationRequest{{Participant: chat.Codex, Task: "invalid core target"}}}, nil
	}
	if err := orchestrator.Post("do not dispatch invalid delegation"); err != nil {
		t.Fatal(err)
	}
	warning := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		warning = warning || (event.Type == EventWarning && strings.Contains(event.Text, "rejected atomically"))
	})
	if !warning {
		t.Fatal("invalid moderator delegation was not rejected atomically")
	}
}

func TestNewHumanSteeringRejectsCancellationResistantStaleResult(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex}
	codex.run = func(_ context.Context, call int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- call
		if call == 1 {
			// Deliberately ignore cancellation to exercise the commit-time epoch
			// check rather than relying on a cooperative provider process.
			<-releaseFirst
			return agent.TurnResult{Text: "stale first result", SessionID: "stale-session", Done: true}, nil
		}
		return agent.TurnResult{Text: "fresh second result", SessionID: "fresh-session", Done: true}, nil
	}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex first request"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not start")
	}
	if err := orchestrator.Post("@codex replacement request"); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	waitForRound(t, orchestrator.Events(), nil)
	roomCopy, messages := orchestrator.Snapshot()
	for _, message := range messages {
		if strings.Contains(message.Text, "stale first result") {
			t.Fatalf("superseded result committed after newer steering: %+v", messages)
		}
	}
	if roomCopy.Sessions[chat.Codex].ID != "fresh-session" {
		t.Fatalf("superseded session overwrote current session: %+v", roomCopy.Sessions[chat.Codex])
	}
}

func TestFailedSteeringAppendPreservesRunningWorkflow(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	controlled := &controlledStore{base: base}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return agent.TurnResult{Text: "original result", SessionID: "original-session", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}}
	orchestrator, err := New(roomState, nil, controlled, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex original request"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("original workflow did not start")
	}
	controlled.failNextAppend()
	if err := orchestrator.Post("@codex steering that cannot persist"); err == nil {
		t.Fatal("steering append unexpectedly succeeded")
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	roomCopy, messages := orchestrator.Snapshot()
	if roomCopy.Sessions[chat.Codex].ID != "original-session" {
		t.Fatalf("original workflow was canceled by failed steering: %+v", roomCopy.Sessions[chat.Codex])
	}
	for _, message := range messages {
		if strings.Contains(message.Text, "steering that cannot persist") {
			t.Fatalf("failed steering leaked into transcript: %+v", messages)
		}
	}
}

func TestSteeringRoomSaveFailureLaunchesNoWorkflow(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	controlled := &controlledStore{base: base}
	started := make(chan struct{}, 1)
	codex := &fakeAgent{participant: chat.Codex, run: func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		started <- struct{}{}
		return agent.TurnResult{Text: "unexpected"}, nil
	}}
	orchestrator, err := New(roomState, nil, controlled, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	controlled.failNextSave()
	if err := orchestrator.Post("@codex persisted message with failed room snapshot"); err == nil {
		t.Fatal("room save unexpectedly succeeded")
	}
	select {
	case <-started:
		t.Fatal("workflow launched after room save failed")
	case <-time.After(100 * time.Millisecond):
	}
	if orchestrator.HasActiveWork() {
		t.Fatal("failed workflow start leaked active-work accounting")
	}
	_, messages := orchestrator.Snapshot()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "persisted message") {
		t.Fatalf("durably appended steering message was not retained: %+v", messages)
	}
}

func TestRecordResultRejectsSupersededMessageAndSessionAtomically(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.mu.Lock()
	version := orchestrator.version
	orchestrator.mu.Unlock()
	orchestrator.Stop()
	if _, err := orchestrator.recordResult(chat.Codex, agent.TurnResult{Text: "stale", SessionID: "stale-session"}, 0, true, version); !errors.Is(err, errWorkflowSuperseded) {
		t.Fatalf("recordResult error=%v, want superseded", err)
	}
	roomCopy, messages := orchestrator.Snapshot()
	if len(messages) != 0 || roomCopy.Sessions[chat.Codex].ID != "" {
		t.Fatalf("superseded result mutated transcript/session: messages=%+v session=%+v", messages, roomCopy.Sessions[chat.Codex])
	}
}

func TestModeratorControlsAreVersionBoundAndAtomic(t *testing.T) {
	newOrchestrator := func(t *testing.T, present bool, controlled *controlledStore) (*Orchestrator, chat.Participant) {
		t.Helper()
		base, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		roomState, err := base.Create(t.TempDir(), 3)
		if err != nil {
			t.Fatal(err)
		}
		worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
		roomState.Members = map[chat.Participant]bool{chat.Codex: true, worker: present}
		var roomStore Store = base
		if controlled != nil {
			controlled.base = base
			roomStore = controlled
		}
		orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: worker})
		if err != nil {
			t.Fatal(err)
		}
		return orchestrator, worker
	}

	t.Run("join plus invalid delegate changes nothing", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		_, err := o.prepareModeratorControls(agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: chat.Codex, Task: "invalid"}}}, map[chat.Participant]bool{}, false, o.version)
		if err == nil || snapshotRoom(o).Present(worker) {
			t.Fatalf("invalid combined marker was partially applied: err=%v members=%+v", err, snapshotRoom(o).Members)
		}
	})

	t.Run("join plus delegate uses projected membership", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		plan, err := o.prepareModeratorControls(agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, map[chat.Participant]bool{}, false, o.version)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshotRoom(o).Present(worker) || len(plan.reserved) != 1 {
			t.Fatalf("valid projected marker was not prepared: plan=%+v room=%+v", plan, snapshotRoom(o).Members)
		}
		o.releaseModeratorControlPlan(plan, false)
	})

	t.Run("leave plus delegate is rejected", func(t *testing.T) {
		o, worker := newOrchestrator(t, true, nil)
		defer o.Close()
		_, err := o.prepareModeratorControls(agent.TurnResult{Leaves: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, map[chat.Participant]bool{}, false, o.version)
		if err == nil || !snapshotRoom(o).Present(worker) {
			t.Fatalf("leave/delegate conflict was partially applied: err=%v room=%+v", err, snapshotRoom(o).Members)
		}
	})

	t.Run("stale and repeated wave controls do not mutate roster", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		version := o.version
		o.Stop()
		if _, err := o.prepareModeratorControls(agent.TurnResult{Joins: []chat.Participant{worker}}, map[chat.Participant]bool{}, false, version); !errors.Is(err, errWorkflowSuperseded) {
			t.Fatalf("stale control error=%v", err)
		}
		if _, err := o.prepareModeratorControls(agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "again"}}}, map[chat.Participant]bool{}, true, o.version); err == nil {
			t.Fatal("repeated delegation wave unexpectedly succeeded")
		}
		if snapshotRoom(o).Present(worker) {
			t.Fatalf("rejected controls changed roster: %+v", snapshotRoom(o).Members)
		}
	})

	t.Run("save failure rolls back roster and reservations", func(t *testing.T) {
		controlled := &controlledStore{}
		o, worker := newOrchestrator(t, false, controlled)
		defer o.Close()
		controlled.failNextSave()
		_, err := o.prepareModeratorControls(agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, map[chat.Participant]bool{}, false, o.version)
		if err == nil {
			t.Fatal("moderator save unexpectedly succeeded")
		}
		o.mu.Lock()
		reserved := o.delegated[worker]
		o.mu.Unlock()
		if snapshotRoom(o).Present(worker) || reserved {
			t.Fatalf("failed moderator transaction leaked state: room=%+v reserved=%v", snapshotRoom(o).Members, reserved)
		}
	})
}

func TestStandaloneDelegationHasScopedCompletionEvent(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, worker: true}
	mainStarted := make(chan struct{}, 1)
	releaseMain := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		mainStarted <- struct{}{}
		select {
		case <-releaseMain:
			return agent.TurnResult{Text: "main result", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}}
	helper := &fakeAgent{participant: worker, run: func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "helper result", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, base, codex, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex main work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mainStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("main work did not start")
	}
	if err := orchestrator.Delegate(worker, "quick check"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventRoundDone {
				t.Fatal("standalone helper emitted room-wide completion")
			}
			if event.Type == EventDelegationDone {
				if event.Participant != worker || !orchestrator.HasActiveWork() {
					t.Fatalf("scoped completion=%+v active=%v", event, orchestrator.HasActiveWork())
				}
				close(releaseMain)
				waitForRound(t, orchestrator.Events(), nil)
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for scoped delegation completion")
		}
	}
}

func TestDelegatedAccessRequestFailsClosedWithoutApproval(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = true
	initialGrantCount := len(roomState.Grants)
	helper := &fakeAgent{participant: worker, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 {
			t.Errorf("delegated permissions=%+v writeRoots=%v", request.Settings, request.WriteRoots)
		}
		return agent.TurnResult{Text: "I tried to request access", AccessRequest: &agent.AccessRequest{Path: "../outside", Mode: chat.AccessReadWrite}}, nil
	}}
	orchestrator, err := New(roomState, nil, base, &fakeAgent{participant: chat.Codex}, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Delegate(worker, "inspect without writes"); err != nil {
		t.Fatal(err)
	}
	sawRejection := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventError && event.Participant == worker && strings.Contains(event.Err.Error(), "attempted to request access") {
				sawRejection = true
			}
			if event.Type == EventDelegationDone {
				if !sawRejection {
					t.Fatal("delegated access request was not rejected")
				}
				roomCopy, _ := orchestrator.Snapshot()
				if len(roomCopy.Grants) != initialGrantCount {
					t.Fatalf("delegated access request changed grants: %+v", roomCopy.Grants)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for delegated access rejection")
		}
	}
}

func TestStopCancelsDelegationAndReleasesWorkerReservation(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = true
	started := make(chan struct{}, 1)
	helper := &fakeAgent{participant: worker, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}}
	orchestrator, err := New(roomState, nil, base, &fakeAgent{participant: chat.Codex}, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Delegate(worker, "wait for cancellation"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("delegation did not start")
	}
	if err := orchestrator.Delegate(worker, "overlap"); err == nil || !strings.Contains(err.Error(), "already working") {
		t.Fatalf("overlapping delegation error=%v", err)
	}
	orchestrator.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for orchestrator.HasActiveWork() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	orchestrator.mu.Lock()
	reserved := orchestrator.delegated[worker]
	orchestrator.mu.Unlock()
	if orchestrator.HasActiveWork() || reserved {
		t.Fatalf("stop leaked delegation accounting: active=%v reserved=%v", orchestrator.HasActiveWork(), reserved)
	}
}

func snapshotRoom(orchestrator *Orchestrator) chat.Room {
	value, _ := orchestrator.Snapshot()
	return value
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

func waitForConflict(t *testing.T, events <-chan Event) {
	t.Helper()
	timeout := time.After(4 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type == EventError {
				t.Fatalf("orchestrator error: %v", event.Err)
			}
			if event.Type == EventConflict {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for conflict")
		}
	}
}
