package room

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	"github.com/timhavens/mohuddle/internal/testutil"
)

type fakeAgent struct {
	participant chat.Participant
	mu          sync.Mutex
	requests    []agent.TurnRequest
	configured  chat.AgentSettings
	resetConfig bool
	resetCalls  int
	run         func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error)
}

type healthAgent struct {
	*fakeAgent
	alive  bool
	reason string
}

func (a *healthAgent) ProcessAlive() (bool, string) { return a.alive, a.reason }

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

type fakeResearcher struct {
	mu       sync.Mutex
	requests []agent.ResearchRequest
	results  []agent.ResearchResult
}

func (r *fakeResearcher) Research(_ context.Context, _ chat.Participant, _ string, requests []agent.ResearchRequest) []agent.ResearchResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, requests...)
	return append([]agent.ResearchResult(nil), r.results...)
}

func (r *fakeResearcher) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
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
func (f *fakeAgent) ResetSession() {
	f.mu.Lock()
	f.resetCalls++
	f.mu.Unlock()
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
func (f *fakeAgent) resetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resetCalls
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
	// The conflict event is emitted just before the workflow goroutine releases
	// its active-work slot. Wait for that safe boundary before exercising the
	// explicit continuation path.
	orchestrator.wg.Wait()
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

func TestLeadBidDeadlineFallsBackToModeratorWithinThreeSeconds(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	blockBid := func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			<-ctx.Done()
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{Text: "moderator fallback", Done: true}, nil
	}
	codexAgent.run = blockBid
	claudeAgent.run = blockBid
	started := time.Now()
	if err := orchestrator.Post("route this promptly"); err != nil {
		t.Fatal(err)
	}
	foundWarning := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventWarning && strings.Contains(event.Text, "configured moderator") {
			foundWarning = true
		}
	})
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("moderator fallback took %s", elapsed)
	}
	if !foundWarning {
		t.Fatal("lead timeout did not report moderator fallback")
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

func TestAskOverlapsUnrelatedWorkspaceWorkflow(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	codexStarted := make(chan struct{}, 1)
	claudeStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	codexAgent.run = func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		codexStarted <- struct{}{}
		<-release
		return agent.TurnResult{Text: "codex work", Done: true}, nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("ask permissions=%s", request.Settings.Permissions)
		}
		claudeStarted <- struct{}{}
		<-release
		return agent.TurnResult{Text: "claude answer", Done: true}, nil
	}
	if err := orchestrator.Post("@codex update the workspace"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-codexStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace workflow did not start")
	}
	if err := orchestrator.Ask("@claude inspect the external evidence"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-claudeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated read-only ask did not overlap workspace workflow")
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
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
	orchestrator.wg.Wait()
	roomState, _ := orchestrator.Snapshot()
	if roomState.Sessions[chat.Agy].ID != "agy-worker-session" || roomState.Sessions[chat.Agy].Cursor == 0 {
		t.Fatalf("elevated AGY session=%+v", roomState.Sessions[chat.Agy])
	}
	wantSession := roomState.Sessions[chat.Agy]
	if err := orchestrator.Ask("@agy discuss without tools"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
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
	extra := testutil.CanonicalTempDir(t)
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
	extra := testutil.CanonicalTempDir(t)
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
	// Keep the cooldown comfortably beyond private lead selection. CI can take
	// long enough for a tiny deadline to expire before the public workflow has
	// started, which tests a different boundary and can strand the blocking fake.
	retryAt := time.Now().Add(time.Hour)
	if err := orchestrator.SetParticipantAvailability(chat.Claude, &chat.ParticipantAvailability{
		Reason: "short test cooldown", Source: "test", DetectedAt: time.Now(), RetryAt: &retryAt, Confidence: "confirmed",
	}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLead := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLead()
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
	retryAt = time.Now().Add(80 * time.Millisecond)
	orchestrator.mu.Lock()
	availability := orchestrator.room.Availability[chat.Claude]
	availability.RetryAt = &retryAt
	orchestrator.room.Availability[chat.Claude] = availability
	orchestrator.mu.Unlock()
	time.Sleep(time.Until(retryAt) + 30*time.Millisecond)
	if err := orchestrator.RefreshCoreState(); err != nil {
		t.Fatal(err)
	}
	if status := orchestrator.CoreStatus(); len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy {
		t.Fatalf("promotion changed mid-workflow: %+v", status)
	}
	releaseLead()
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
	// Keep the retry well beyond setup. On a loaded CI host, a tiny initial
	// cooldown can expire before Post has acquired the active-work slot, which
	// tests pre-work execution rather than the safe active-work boundary.
	retryAt := time.Now().Add(time.Hour).UTC()
	roomState.Availability = make(map[chat.Participant]chat.ParticipantAvailability)
	roomState.Availability[worker] = chat.ParticipantAvailability{Reason: "quota", Source: "test", DetectedAt: time.Now().UTC(), RetryAt: &retryAt, Confidence: "confirmed"}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWork := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWork()
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
	retryAt = time.Now().Add(70 * time.Millisecond).UTC()
	orchestrator.mu.Lock()
	availability := orchestrator.room.Availability[worker]
	availability.RetryAt = &retryAt
	orchestrator.room.Availability[worker] = availability
	orchestrator.mu.Unlock()
	orchestrator.signalRosterScheduler()
	time.Sleep(100 * time.Millisecond)
	current, _ := orchestrator.Snapshot()
	if current.Present(worker) || current.RosterActions[0].Status != chat.RosterActionPending {
		t.Fatalf("scheduled action ran during active workflow: %+v", current.RosterActions)
	}
	releaseWork()
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
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, codexOne: true, claudeOne: true}
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
		t.Fatalf("same-provider delegation should use the second configured provider slot: %v", err)
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
			t.Fatal("eligible alternate-provider worker did not overlap the active main turn")
		}
	}
	if !seen[codexOne] || !seen[claudeOne] {
		t.Fatalf("unexpected overlapping workers: %+v", seen)
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
	found := false
	for _, message := range messages {
		found = found || message.Author == claudeOne
		found = found || message.Author == codexOne
	}
	if !found {
		t.Fatalf("no public result attributed to %s: %+v", claudeOne, messages)
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
	worker, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, worker: true}
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
	if start < 0 || len(request.Prompt[start:]) > maxAuxiliaryTranscriptBytes {
		t.Fatalf("delegated transcript length=%d, limit=%d", len(request.Prompt[start:]), maxAuxiliaryTranscriptBytes)
	}
}

func TestColdAuxiliaryRoundReceivesBoundedRecentContext(t *testing.T) {
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
	for sequence := uint64(1); sequence <= 2300; sequence++ {
		orchestrator.messages = append(orchestrator.messages, chat.Message{
			Sequence: sequence, Author: chat.Codex, Kind: chat.MessageTool,
			Text: fmt.Sprintf("room-record-%d %s", sequence, strings.Repeat("x", 512)),
		})
	}
	orchestrator.nextSequence = 2301
	orchestrator.mu.Unlock()

	request := orchestrator.turnRequest(worker, turnSpec{
		through: 2300, readOnly: true, coreParticipants: []chat.Participant{chat.Codex},
		instruction: "Give a two-sentence introduction.",
	}, nil)
	if strings.Contains(request.Prompt, "room-record-1 ") {
		t.Fatal("cold auxiliary round replayed the oldest room history")
	}
	if !strings.Contains(request.Prompt, "room-record-2300 ") {
		t.Fatal("cold auxiliary round discarded the newest room history")
	}
	start := strings.Index(request.Prompt, "BEGIN UNTRUSTED ROOM TRANSCRIPT")
	if start < 0 || len(request.Prompt[start:]) > maxAuxiliaryTranscriptBytes {
		t.Fatalf("auxiliary transcript length=%d, limit=%d", len(request.Prompt[start:]), maxAuxiliaryTranscriptBytes)
	}
	if len(request.Prompt) >= 128*1024 {
		t.Fatalf("auxiliary request unexpectedly large: %d bytes", len(request.Prompt))
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
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, codexOne: false, claudeOne: false}
	roomState.DelegationPolicy = chat.DelegationAuto
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

func TestAskDelegationWaitsWithoutReservationsThenRunsApprovedSplit(t *testing.T) {
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
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, codexOne: true, claudeOne: true}
	roomState.DelegationPolicy = chat.DelegationAsk
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	worker := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if len(request.WriteRoots) != 0 || request.Settings.Permissions != chat.PermissionReadOnly {
				t.Errorf("delegated %s turn was not read-only: %+v", participant, request)
			}
			started <- participant
			<-release
			return agent.TurnResult{Text: string(participant) + " result", Done: true}, nil
		}}
	}
	leadCalls := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "proposed split", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: codexOne, Task: "inspect parsing"},
				{Participant: claudeOne, Task: "inspect persistence"},
			}}, nil
		}
		if !strings.Contains(request.Prompt, "codex-1 result") || !strings.Contains(request.Prompt, "claude-1 result") {
			t.Errorf("approved split results were not returned to requester: %s", request.Prompt)
		}
		if request.Settings.Permissions != chat.PermissionWorkspace || len(request.WriteRoots) == 0 {
			t.Errorf("execution lead did not regain its write-capable turn: permissions=%q roots=%v", request.Settings.Permissions, request.WriteRoots)
		}
		return agent.TurnResult{Text: "synthesized", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, worker(codexOne), worker(claudeOne))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("implement the parser and persistence changes"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type != EventDelegationAsk {
				continue
			}
			roomCopy, _ := orchestrator.Snapshot()
			if roomCopy.PendingDelegation == nil || len(roomCopy.PendingDelegation.Tasks) != 2 || !roomCopy.PendingDelegation.AttemptCharged || roomCopy.PendingDelegation.ProviderLanes != 2 {
				t.Fatalf("pending delegation=%+v", roomCopy.PendingDelegation)
			}
			orchestrator.mu.Lock()
			reservedTargets := len(orchestrator.delegated)
			state := orchestrator.delegationStates[orchestrator.version]
			attempts, boundaries, tasks := 0, 0, 0
			if state != nil {
				attempts, boundaries, tasks = state.attempts, state.boundaries, state.tasks
			}
			orchestrator.mu.Unlock()
			if reservedTargets != 0 {
				t.Fatalf("ask mode held participant reservations: targets=%d", reservedTargets)
			}
			if attempts != 1 || boundaries != 0 || tasks != 0 {
				t.Fatalf("preview accounting attempts=%d boundaries=%d tasks=%d", attempts, boundaries, tasks)
			}
			if err := orchestrator.ApprovePendingDelegation(); err != nil {
				t.Fatal(err)
			}
			goto approved
		case <-time.After(2 * time.Second):
			t.Fatal("delegation approval prompt was not emitted")
		}
	}

approved:
	seen := make(map[chat.Participant]bool)
	for len(seen) < 2 {
		select {
		case participant := <-started:
			seen[participant] = true
		case <-time.After(2 * time.Second):
			t.Fatal("approved delegates did not start")
		}
	}
	orchestrator.mu.Lock()
	state := orchestrator.delegationStates[orchestrator.version]
	attempts, boundaries, tasks := state.attempts, state.boundaries, state.tasks
	orchestrator.mu.Unlock()
	if attempts != 1 || boundaries != 1 || tasks != 2 {
		t.Fatalf("approval double-charged proposal: attempts=%d boundaries=%d tasks=%d", attempts, boundaries, tasks)
	}
	close(release)
	waitForRound(t, orchestrator.Events(), nil)
	roomCopy, _ := orchestrator.Snapshot()
	if roomCopy.PendingDelegation != nil || leadCalls != 2 {
		t.Fatalf("pending=%+v lead calls=%d", roomCopy.PendingDelegation, leadCalls)
	}
}

func TestAskDelegationRunSoloNeverStartsWorker(t *testing.T) {
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
	roomState.DelegationPolicy = chat.DelegationAsk
	leadCalls := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "considering a split", Done: false, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect parser"}}}, nil
		}
		if !strings.Contains(request.Prompt, "Run solo") {
			t.Errorf("solo decision was not returned to requester: %s", request.Prompt)
		}
		if leadCalls > 2 {
			t.Errorf("Run solo reopened delegation; lead calls=%d", leadCalls)
		}
		return agent.TurnResult{Text: "completed solo", Done: true, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "reproposed task"}}}, nil
	}}
	helper := &fakeAgent{participant: worker}
	orchestrator, err := New(roomState, nil, roomStore, codex, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("implement the parser change"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventDelegationAsk {
				if err := orchestrator.DeclinePendingDelegation(); err != nil {
					t.Fatal(err)
				}
				goto declined
			}
		case <-time.After(2 * time.Second):
			t.Fatal("delegation approval prompt was not emitted")
		}
	}

declined:
	waitForRound(t, orchestrator.Events(), nil)
	if helper.callCount() != 0 || leadCalls != 2 {
		t.Fatalf("worker calls=%d lead calls=%d", helper.callCount(), leadCalls)
	}
}

func TestStopDuringDelegationApprovalTerminatesWait(t *testing.T) {
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
	roomState.DelegationPolicy = chat.DelegationAsk
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		return agent.TurnResult{Text: "proposal", Done: false, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, nil
	}}
	helper := &fakeAgent{participant: worker}
	orchestrator, err := New(roomState, nil, roomStore, lead, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("inspect cancellation"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type != EventDelegationAsk {
				continue
			}
			orchestrator.Stop()
			goto stopped
		case <-time.After(2 * time.Second):
			t.Fatal("delegation approval prompt was not emitted")
		}
	}

stopped:
	deadline := time.Now().Add(2 * time.Second)
	for orchestrator.HasActiveWork() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	roomCopy, _ := orchestrator.Snapshot()
	if orchestrator.HasActiveWork() || roomCopy.PendingDelegation != nil || leadCalls != 1 || helper.callCount() != 0 {
		t.Fatalf("stop cleanup active=%v pending=%+v lead=%d worker=%d", orchestrator.HasActiveWork(), roomCopy.PendingDelegation, leadCalls, helper.callCount())
	}
}

func TestSteeringSupersedesDelegationApprovalWithoutResumingStaleRequester(t *testing.T) {
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
	roomState.DelegationPolicy = chat.DelegationAsk
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "proposal", Done: false, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, nil
		}
		if !strings.Contains(request.Prompt, "replacement request") {
			t.Errorf("superseding turn did not receive replacement request: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "replacement complete", Done: true}, nil
	}}
	helper := &fakeAgent{participant: worker}
	orchestrator, err := New(roomState, nil, roomStore, lead, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("inspect supersession"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type != EventDelegationAsk {
				continue
			}
			if err := orchestrator.Steer("@codex replacement request"); err != nil {
				t.Fatal(err)
			}
			goto superseded
		case <-time.After(2 * time.Second):
			t.Fatal("delegation approval prompt was not emitted")
		}
	}

superseded:
	waitForRound(t, orchestrator.Events(), nil)
	roomCopy, _ := orchestrator.Snapshot()
	if roomCopy.PendingDelegation != nil || leadCalls != 2 || helper.callCount() != 0 {
		t.Fatalf("supersession pending=%+v lead=%d worker=%d", roomCopy.PendingDelegation, leadCalls, helper.callCount())
	}
}

func TestHostRestartClearsUnresumablePendingDelegation(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.PendingDelegation = &chat.PendingDelegation{
		ID: "stale", WorkflowVersion: 7, SourceSequence: 2, Requester: chat.Codex,
		Tasks: []chat.DelegationTask{{Participant: chat.Claude, Task: "inspect"}}, CreatedAt: time.Now().UTC(),
	}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, nil, roomStore, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	roomCopy, _ := orchestrator.Snapshot()
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if roomCopy.PendingDelegation != nil || loaded.PendingDelegation != nil {
		t.Fatalf("stale pending delegation survived restart: memory=%+v disk=%+v", roomCopy.PendingDelegation, loaded.PendingDelegation)
	}
}

func TestLegacyPendingInputMigratesToDurableWorkflowWithoutTranscriptRewrite(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.SchemaVersion = 0
	roomState.Workflows = nil
	roomState.InputResolutions = nil
	roomState.PendingInputs = []uint64{1}
	message := chat.Message{
		ID: "legacy", Sequence: 1, Author: chat.User, Target: chat.Codex, Kind: chat.MessageText,
		WorkflowMode: chat.WorkflowExecute, DelegationPolicy: chat.DelegationManual, Text: "legacy queued work", CreatedAt: time.Now().UTC(),
	}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	if err := roomStore.AppendMessage(roomState.ID, message); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, []chat.Message{message}, roomStore, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	migrated, messages := orchestrator.Snapshot()
	resolution := migrated.InputResolutions[1]
	if migrated.SchemaVersion != chat.CurrentRoomSchemaVersion || len(migrated.Workflows) != 1 || resolution.WorkflowID == "" || migrated.Workflows[resolution.WorkflowID].State != chat.WorkflowQueued {
		t.Fatalf("migration room=%+v", migrated)
	}
	if len(messages) != 1 || messages[0].WorkflowID != "" || messages[0].Text != message.Text {
		t.Fatalf("migration rewrote append-only transcript: %+v", messages)
	}
	persisted, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.InputResolutions[1].WorkflowID != resolution.WorkflowID || len(persisted.PendingInputs) != 1 {
		t.Fatalf("persisted migration=%+v", persisted)
	}
}

func TestHostRestartRestoresPendingAutomaticRecovery(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := chat.Message{
		ID: "source", Sequence: 1, WorkflowID: "workflow", Author: chat.User, Kind: chat.MessageText,
		WorkflowMode: chat.WorkflowExecute, DelegationPolicy: chat.DelegationManual, InputIntent: chat.InputWork,
		Text: "implement the recovery", CreatedAt: now,
	}
	roomState.Workflows["workflow"] = chat.WorkflowRecord{
		ID: "workflow", Generation: 1, SourceSequences: []uint64{source.Sequence}, Target: chat.Codex,
		Mode: chat.WorkflowExecute, DelegationPolicy: chat.DelegationManual, Resource: chat.WorkflowWorkspaceWrite,
		State: chat.WorkflowWaiting, RecoveryAttempts: 1, RecoveryReason: "repeated command", RecoveryActors: []chat.Participant{chat.Codex},
		RecoveryTarget: chat.Claude, RecoveryPending: true, RecoveryAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	if err := roomStore.AppendMessage(roomState.ID, source); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(roomState, []chat.Message{source}, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	restored, _ := orchestrator.Snapshot()
	record := restored.Workflows["workflow"]
	if len(restored.PendingInputs) != 1 || restored.PendingInputs[0] != source.Sequence || record.State != chat.WorkflowQueued || !record.RecoveryPending || record.RecoveryAttempts != 1 || record.RecoveryTarget != chat.Claude || record.RecoveryAt == nil {
		t.Fatalf("pending recovery was not restored: room=%+v workflow=%+v", restored.PendingInputs, record)
	}
	persisted, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Workflows["workflow"].RecoveryPending || len(persisted.PendingInputs) != 1 {
		t.Fatalf("restored recovery was not durable: %+v", persisted.Workflows["workflow"])
	}
}

func TestDelegationBatchUsesConfiguredSameProviderCapacityAndOverlapsProviders(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	codexOne, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	codexTwo, _ := chat.AuxiliaryParticipant(chat.Codex, 2)
	claudeOne, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, codexOne: true, codexTwo: true, claudeOne: true}
	roomState.DelegationPolicy = chat.DelegationAuto
	var mu sync.Mutex
	active := make(map[chat.Participant]int)
	maxActive := make(map[chat.Participant]int)
	totalActive, maxTotal := 0, 0
	worker := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			provider := participant.Provider()
			mu.Lock()
			active[provider]++
			totalActive++
			maxActive[provider] = max(maxActive[provider], active[provider])
			maxTotal = max(maxTotal, totalActive)
			mu.Unlock()
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			active[provider]--
			totalActive--
			mu.Unlock()
			return agent.TurnResult{Text: string(participant) + " result", Done: true}, nil
		}}
	}
	leadCalls := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "split", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: codexOne, Task: "one"}, {Participant: codexTwo, Task: "two"}, {Participant: claudeOne, Task: "three"},
			}}, nil
		}
		return agent.TurnResult{Text: "synthesis", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, worker(codexOne), worker(codexTwo), worker(claudeOne))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("implement three independent checks"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	mu.Lock()
	defer mu.Unlock()
	if maxActive[chat.Codex] != 2 || maxActive[chat.Claude] != 1 || maxTotal < 3 {
		t.Fatalf("provider concurrency max=%v total=%d", maxActive, maxTotal)
	}
}

func TestAutomaticDelegationRunsSameProviderSplitWithinCapacity(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	two, _ := chat.AuxiliaryParticipant(chat.Codex, 2)
	roomState.Members[one], roomState.Members[two] = true, true
	roomState.DelegationPolicy = chat.DelegationAuto
	leadCalls := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "single-provider split", Done: false, Delegates: []agent.DelegationRequest{{Participant: one, Task: "one"}, {Participant: two, Task: "two"}}}, nil
		}
		if !strings.Contains(request.Prompt, "The delegated results are now in the transcript") {
			t.Errorf("delegated results were not returned to lead: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "solo result", Done: true}, nil
	}}
	first, second := &fakeAgent{participant: one}, &fakeAgent{participant: two}
	orchestrator, err := New(roomState, nil, roomStore, codex, first, second)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("implement two checks"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if first.callCount() != 1 || second.callCount() != 1 || leadCalls != 2 {
		t.Fatalf("workers=%d/%d lead=%d", first.callCount(), second.callCount(), leadCalls)
	}
}

func TestParallelTargetedDirectLeadCanDelegate(t *testing.T) {
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
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, codexOne: true, claudeOne: true}
	leadCalls := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			if !strings.Contains(request.SystemPrompt, "Delegation policy: AUTO") {
				t.Errorf("direct lead did not receive auto capability: %s", request.SystemPrompt)
			}
			return agent.TurnResult{Text: "direct split", Done: false, Delegates: []agent.DelegationRequest{{Participant: codexOne, Task: "inspect parser"}, {Participant: claudeOne, Task: "inspect store"}}}, nil
		}
		return agent.TurnResult{Text: "direct synthesis", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, &fakeAgent{participant: codexOne}, &fakeAgent{participant: claudeOne})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.PostParallelWithAttachments("@codex implement parser and store changes", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if leadCalls != 2 {
		t.Fatalf("direct lead calls=%d", leadCalls)
	}
}

func TestDelegationPolicyIsResolvedAndStampedAtSubmission(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.SetDelegationPolicy(chat.DelegationAdaptive); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PostSoloWithAttachments("implement the solo parser change", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if err := orchestrator.PostParallelWithAttachments("implement independent parser and store checks", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("design parser and store changes"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if err := orchestrator.SetWorkflowMode(chat.WorkflowExecute); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex implement the targeted parser change"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if err := orchestrator.Post("implement the ordinary parser change"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	_, messages := orchestrator.Snapshot()
	var policies []chat.DelegationPolicy
	for _, message := range messages {
		if message.Author == chat.User {
			policies = append(policies, message.DelegationPolicy)
		}
	}
	want := []chat.DelegationPolicy{chat.DelegationManual, chat.DelegationAuto, chat.DelegationAuto, chat.DelegationAsk, chat.DelegationAsk}
	if fmt.Sprint(policies) != fmt.Sprint(want) {
		t.Fatalf("stamped policies=%v want=%v", policies, want)
	}
}

func TestDelegatedCorePeerStillRunsScheduledReview(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	agyOne, _ := chat.AuxiliaryParticipant(chat.Agy, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, agyOne: true}
	roomState.DelegationPolicy = chat.DelegationAuto
	leadTurns := 0
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		leadTurns++
		if leadTurns == 1 {
			return agent.TurnResult{Text: "delegate checks", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: chat.Claude, Task: "inspect parser"}, {Participant: agyOne, Task: "inspect persistence"},
			}}, nil
		}
		return agent.TurnResult{Text: "codex synthesis", Done: true}, nil
	}}
	claudeDelegated, claudeReviewed := false, false
	claude := &fakeAgent{participant: chat.Claude, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Codex), nil
		}
		if strings.Contains(request.SystemPrompt, "assigned you this independent subtask") {
			claudeDelegated = true
			return agent.TurnResult{Text: "claude delegated result", Done: true}, nil
		}
		if strings.Contains(request.SystemPrompt, "Review the lead's response") {
			claudeReviewed = true
		}
		return agent.TurnResult{Done: true}, nil
	}}
	agy := &fakeAgent{participant: agyOne, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "agy delegated result", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, claude, agy)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("implement parser and persistence changes"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if !claudeDelegated || !claudeReviewed {
		t.Fatalf("claude delegated=%v reviewed=%v", claudeDelegated, claudeReviewed)
	}
}

func TestReviewerDelegationMarkerIsIgnoredByHostAuthority(t *testing.T) {
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
	reviewer := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		return agent.TurnResult{Text: "review", Done: false, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "forged task"}}}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, reviewer, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	outcome := orchestrator.runOne(chat.Codex, orchestrator.version, turnSpec{through: orchestrator.latestSequence(), readOnly: true, role: "reviewer", instruction: "Review only."})
	if len(outcome.result.Delegates) != 0 {
		t.Fatalf("unauthorized delegates survived: %+v", outcome.result.Delegates)
	}
	warning := false
	deadline := time.After(time.Second)
	for !warning {
		select {
		case event := <-orchestrator.Events():
			warning = event.Type == EventWarning && strings.Contains(event.Text, "lacks delegation authority")
		case <-deadline:
			t.Fatal("unauthorized delegation warning was not emitted")
		}
	}
}

func TestWorkflowDelegationBudgetAllowsTwoBoundariesAndFourTasks(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	participants := make([]chat.Participant, 0, 4)
	agents := []agent.Agent{&fakeAgent{participant: chat.Codex}}
	for _, provider := range []chat.Participant{chat.Codex, chat.Claude, chat.Agy, chat.Copilot} {
		worker, _ := chat.AuxiliaryParticipant(provider, 1)
		participants = append(participants, worker)
		roomState.Members[provider] = true
		roomState.Members[worker] = true
		agents = append(agents, &fakeAgent{participant: worker})
	}
	orchestrator, err := New(roomState, nil, roomStore, agents...)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	makeOutcome := func(tasks []agent.DelegationRequest) turnOutcome {
		return turnOutcome{participant: chat.Codex, result: agent.TurnResult{Done: false, Delegates: tasks}, authority: turnAuthority{
			participant: chat.Codex, workflowVersion: orchestrator.version, role: "lead", mayDelegate: true, policy: chat.DelegationAuto,
		}}
	}
	first, err := orchestrator.prepareTurnControls(makeOutcome([]agent.DelegationRequest{{Participant: participants[0], Task: "one"}, {Participant: participants[1], Task: "two"}}), true, true)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator.releaseTurnControlPlan(first, false)
	second, err := orchestrator.prepareTurnControls(makeOutcome([]agent.DelegationRequest{{Participant: participants[2], Task: "three"}, {Participant: participants[3], Task: "four"}}), true, true)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator.releaseTurnControlPlan(second, false)
	if _, err := orchestrator.prepareTurnControls(makeOutcome([]agent.DelegationRequest{{Participant: participants[0], Task: "five"}}), true, true); err == nil || !strings.Contains(err.Error(), "boundary limit") {
		t.Fatalf("third boundary error=%v", err)
	}
	orchestrator.mu.Lock()
	state := orchestrator.delegationStates[orchestrator.version]
	orchestrator.mu.Unlock()
	if state == nil || state.boundaries != 2 || state.tasks != 4 {
		t.Fatalf("delegation state=%+v", state)
	}
}

func TestRejectedDelegationProposalsTerminateAtAttemptLimit(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, worker: true}
	roomState.DelegationPolicy = chat.DelegationAuto
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		return agent.TurnResult{Text: "invalid split", Done: false, Delegates: []agent.DelegationRequest{
			{Participant: chat.Codex, Task: "self task"},
			{Participant: worker, Task: "worker task"},
		}}, nil
	}}
	helper := &fakeAgent{participant: worker}
	orchestrator, err := New(roomState, nil, roomStore, lead, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.PostParallelWithAttachments("@codex inspect invalid delegation handling", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if leadCalls != maxDelegationAttempts+1 {
		t.Fatalf("lead calls=%d, want %d attempts plus one terminal continuation", leadCalls, maxDelegationAttempts+1)
	}
	if helper.callCount() != 0 {
		t.Fatalf("invalid proposal started worker %d time(s)", helper.callCount())
	}
	orchestrator.mu.Lock()
	reservedTargets := len(orchestrator.delegated)
	orchestrator.mu.Unlock()
	if reservedTargets != 0 {
		t.Fatalf("rejected proposals leaked reservations: targets=%d", reservedTargets)
	}
}

func TestInvalidProposalStillAllowsTwoDispatchedWaves(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	workers := make([]chat.Participant, 0, 4)
	agents := make([]agent.Agent, 0, 5)
	for _, provider := range []chat.Participant{chat.Codex, chat.Claude, chat.Agy, chat.Copilot} {
		worker, _ := chat.AuxiliaryParticipant(provider, 1)
		workers = append(workers, worker)
		roomState.Members[provider] = true
		roomState.Members[worker] = true
		agents = append(agents, &fakeAgent{participant: worker})
	}
	roomState.DelegationPolicy = chat.DelegationAuto
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		switch leadCalls {
		case 1:
			return agent.TurnResult{Text: "invalid first proposal", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: chat.Codex, Task: "self task"}, {Participant: workers[1], Task: "unused task"},
			}}, nil
		case 2:
			return agent.TurnResult{Text: "corrected first wave", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: workers[0], Task: "wave one A"}, {Participant: workers[1], Task: "wave one B"},
			}}, nil
		case 3:
			return agent.TurnResult{Text: "second wave", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: workers[2], Task: "wave two A"}, {Participant: workers[3], Task: "wave two B"},
			}}, nil
		default:
			return agent.TurnResult{Text: "terminal synthesis", Done: false, Delegates: []agent.DelegationRequest{{Participant: workers[0], Task: "must be ignored"}}}, nil
		}
	}}
	agents = append([]agent.Agent{lead}, agents...)
	orchestrator, err := New(roomState, nil, roomStore, agents...)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.PostParallelWithAttachments("@codex use two useful waves", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if leadCalls != 4 {
		t.Fatalf("lead calls=%d, want invalid attempt, two waves, and terminal synthesis", leadCalls)
	}
	for index, participant := range workers {
		if calls := agents[index+1].(*fakeAgent).callCount(); calls != 1 {
			t.Fatalf("%s calls=%d, want 1", participant, calls)
		}
	}
}

func TestSuccessfulWaveThenRejectedProposalsTerminateCleanly(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	second, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	unused, _ := chat.AuxiliaryParticipant(chat.Agy, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, first: true, second: true, unused: true}
	roomState.DelegationPolicy = chat.DelegationAuto
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		if leadCalls == 1 {
			return agent.TurnResult{Text: "valid first wave", Done: false, Delegates: []agent.DelegationRequest{
				{Participant: first, Task: "first"}, {Participant: second, Task: "second"},
			}}, nil
		}
		return agent.TurnResult{Text: "invalid follow-up", Done: false, Delegates: []agent.DelegationRequest{
			{Participant: chat.Codex, Task: "self task"}, {Participant: unused, Task: "unused"},
		}}, nil
	}}
	firstAgent, secondAgent, unusedAgent := &fakeAgent{participant: first}, &fakeAgent{participant: second}, &fakeAgent{participant: unused}
	orchestrator, err := New(roomState, nil, roomStore, lead, firstAgent, secondAgent, unusedAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.PostParallelWithAttachments("@codex run one wave then recover", nil); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if leadCalls != maxDelegationAttempts+1 {
		t.Fatalf("lead calls=%d, want one wave, two rejected proposals, and terminal continuation", leadCalls)
	}
	if firstAgent.callCount() != 1 || secondAgent.callCount() != 1 || unusedAgent.callCount() != 0 {
		t.Fatalf("worker calls first=%d second=%d unused=%d", firstAgent.callCount(), secondAgent.callCount(), unusedAgent.callCount())
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

func TestExplicitSteeringRejectsCancellationResistantStaleResult(t *testing.T) {
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
	if err := orchestrator.Steer("@codex replacement request"); err != nil {
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

func TestNormalHumanInputQueuesWithoutCancelingAndRunsAtSafeBoundary(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan int, 3)
	releaseFirst := make(chan struct{})
	firstCanceled := make(chan struct{}, 1)
	secondPrompt := make(chan string, 1)
	codex := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- call
		if call == 1 {
			select {
			case <-releaseFirst:
				return agent.TurnResult{Text: "first complete", SessionID: "first-session", Done: true}, nil
			case <-ctx.Done():
				firstCanceled <- struct{}{}
				return agent.TurnResult{}, ctx.Err()
			}
		}
		secondPrompt <- request.Prompt
		return agent.TurnResult{Text: "follow-up complete", SessionID: "second-session", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex first request"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := orchestrator.Post("@codex queued follow-up"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex second queued detail"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Ask("@codex should not supersede"); err == nil || !strings.Contains(err.Error(), "active call") {
		t.Fatalf("active /ask error=%v", err)
	}
	roomCopy, _ := orchestrator.Snapshot()
	if len(roomCopy.PendingInputs) != 2 || orchestrator.PendingInputCount() != 2 {
		t.Fatalf("pending inputs=%v", roomCopy.PendingInputs)
	}
	select {
	case <-firstCanceled:
		t.Fatal("ordinary input canceled the active turn")
	case call := <-started:
		t.Fatalf("queued turn started before safe boundary: call=%d", call)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case call := <-started:
		if call != 2 {
			t.Fatalf("next call=%d want=2", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued turn did not start")
	}
	prompt := <-secondPrompt
	if !strings.Contains(prompt, "queued follow-up") || !strings.Contains(prompt, "second queued detail") {
		t.Fatalf("queued prompt missing follow-up: %s", prompt)
	}
	orchestrator.wg.Wait()
	roomCopy, messages := orchestrator.Snapshot()
	if len(roomCopy.PendingInputs) != 0 || roomCopy.Sessions[chat.Codex].ID != "second-session" {
		t.Fatalf("final room=%+v", roomCopy)
	}
	if codex.resetCount() != 0 {
		t.Fatalf("same-workflow addendum reset provider session %d time(s)", codex.resetCount())
	}
	for _, expected := range []string{"first complete", "follow-up complete"} {
		found := false
		for _, message := range messages {
			found = found || strings.Contains(message.Text, expected)
		}
		if !found {
			t.Fatalf("missing %q in messages=%+v", expected, messages)
		}
	}
	if err := orchestrator.PostNew("@codex separate request"); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-started:
		if call != 3 {
			t.Fatalf("separate workflow call=%d want=3", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("separate workflow did not start")
	}
	if prompt := <-secondPrompt; !strings.Contains(prompt, "separate request") {
		t.Fatalf("separate workflow prompt=%q", prompt)
	}
	orchestrator.wg.Wait()
	if codex.resetCount() != 1 {
		t.Fatalf("genuine workflow switch reset provider session %d time(s), want 1", codex.resetCount())
	}
}

func TestTargetedFollowUpReusesWorkflowAndRunsAtProviderBoundary(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan agent.TurnRequest, 2)
	releaseFirst := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- request
		if call == 1 {
			<-releaseFirst
		}
		return agent.TurnResult{Text: fmt.Sprintf("answer %d", call), SessionID: "session", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex first request"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := orchestrator.Post("@codex corrective follow-up"); err != nil {
		t.Fatal(err)
	}
	roomCopy, messages := orchestrator.Snapshot()
	if len(messages) != 2 || messages[0].WorkflowID == "" || messages[0].WorkflowID != messages[1].WorkflowID {
		t.Fatalf("follow-up lifecycle=%+v", messages)
	}
	record := roomCopy.Workflows[messages[0].WorkflowID]
	if len(roomCopy.PendingInputs) != 1 || record.WaitReason != "addendum pending at provider boundary" {
		t.Fatalf("follow-up queue=%v record=%+v", roomCopy.PendingInputs, record)
	}
	close(releaseFirst)
	second := <-started
	if !strings.Contains(second.Prompt, "corrective follow-up") {
		t.Fatalf("follow-up prompt=%s", second.Prompt)
	}
	orchestrator.wg.Wait()
	roomCopy, _ = orchestrator.Snapshot()
	record = roomCopy.Workflows[messages[0].WorkflowID]
	if record.Generation != 2 || record.State != chat.WorkflowCompleted {
		t.Fatalf("follow-up completion=%+v", record)
	}
}

func TestIndependentPlanWorkflowsOverlapAcrossTargets(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	blocking := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			<-release
			return agent.TurnResult{Text: string(participant) + " plan", Done: true}, nil
		}}
	}
	orchestrator, err := New(roomState, nil, roomStore, blocking(chat.Codex), blocking(chat.Claude))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex inspect parser"); err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != chat.Codex {
		t.Fatalf("first=%s", got)
	}
	if err := orchestrator.Post("@claude inspect persistence"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != chat.Claude {
			t.Fatalf("second=%s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("independent read-only workflow did not overlap")
	}
	close(release)
	orchestrator.wg.Wait()
}

func TestWorkspaceWriteLeaseSerializesIndependentTargets(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan chat.Participant, 2)
	release := make(chan struct{})
	blocking := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			if participant == chat.Codex {
				<-release
			}
			return agent.TurnResult{Text: string(participant) + " result", Done: true}, nil
		}}
	}
	orchestrator, err := New(roomState, nil, roomStore, blocking(chat.Codex), blocking(chat.Claude))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex change parser"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := orchestrator.Post("@claude change persistence"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		t.Fatalf("writer %s overlapped canonical checkout lease", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-started:
		if got != chat.Claude {
			t.Fatalf("queued writer=%s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer did not start after lease release")
	}
	orchestrator.wg.Wait()
}

func TestRunnableReadOnlyWorkflowSkipsBlockedWriterAtQueueHead(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	writerStarted := make(chan struct{}, 1)
	secondWriterStarted := make(chan struct{}, 1)
	readerStarted := make(chan struct{}, 1)
	releaseWriter := make(chan struct{})
	agents[chat.Codex].run = func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		writerStarted <- struct{}{}
		<-releaseWriter
		return agent.TurnResult{Text: "first writer complete", Done: true}, nil
	}
	agents[chat.Claude].run = func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		secondWriterStarted <- struct{}{}
		return agent.TurnResult{Text: "second writer complete", Done: true}, nil
	}
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("queued reader permissions=%s", request.Settings.Permissions)
		}
		readerStarted <- struct{}{}
		return agent.TurnResult{Text: "reader complete", Done: true}, nil
	}
	if err := orchestrator.PostNew("@codex first writer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first writer did not start")
	}
	if err := orchestrator.PostNew("@claude second writer"); err != nil {
		t.Fatal(err)
	}
	orchestrator.mu.Lock()
	orchestrator.room.WorkflowMode = chat.WorkflowPlan
	_, busyCancel := context.WithCancel(context.Background())
	orchestrator.activeTurns[chat.Agy] = activeTurn{cancel: busyCancel}
	orchestrator.mu.Unlock()
	if err := orchestrator.PostNew("@agy independent inspection"); err != nil {
		t.Fatal(err)
	}
	orchestrator.mu.Lock()
	delete(orchestrator.activeTurns, chat.Agy)
	orchestrator.mu.Unlock()
	busyCancel()
	if err := orchestrator.ResumeQueued(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runnable reader remained blocked behind workspace writer")
	}
	select {
	case <-secondWriterStarted:
		t.Fatal("second writer bypassed canonical checkout lease")
	case <-time.After(100 * time.Millisecond):
	}
	roomState, _ := orchestrator.Snapshot()
	if len(roomState.PendingInputs) != 1 {
		t.Fatalf("pending inputs=%v want only blocked writer", roomState.PendingInputs)
	}
	close(releaseWriter)
	select {
	case <-secondWriterStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked writer did not start after lease release")
	}
	orchestrator.wg.Wait()
}

func TestSingleHelperRetainedLeadWorkOverlapsBeforeSynthesis(t *testing.T) {
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
	roomState.DelegationPolicy = chat.DelegationAuto
	started := make(chan string, 2)
	release := make(chan struct{})
	leadCalls := 0
	lead := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		leadCalls++
		switch leadCalls {
		case 1:
			return agent.TurnResult{Text: "split", Done: false, RetainedTask: "inspect scheduler", Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect persistence"}}}, nil
		case 2:
			if !strings.Contains(request.SystemPrompt, "Retained task: inspect scheduler") {
				t.Errorf("retained prompt=%s", request.SystemPrompt)
			}
			started <- "lead"
			<-release
			return agent.TurnResult{Text: "lead progress", Done: true}, nil
		default:
			return agent.TurnResult{Text: "final synthesis", Done: true}, nil
		}
	}}
	helper := &fakeAgent{participant: worker, run: func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- "helper"
		<-release
		return agent.TurnResult{Text: "helper result", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, lead, helper)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex inspect both areas"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case value := <-started:
			seen[value] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("retained/helper overlap=%v", seen)
		}
	}
	close(release)
	orchestrator.wg.Wait()
	if leadCalls != 3 || helper.callCount() != 1 {
		t.Fatalf("lead=%d helper=%d", leadCalls, helper.callCount())
	}
}

func TestScopedStopCancelsOnlySelectedWorkflow(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan chat.Participant, 2)
	cancelled := make(chan chat.Participant, 2)
	releaseClaude := make(chan struct{})
	blocking := func(participant chat.Participant) *fakeAgent {
		return &fakeAgent{participant: participant, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			if participant == chat.Claude {
				select {
				case <-releaseClaude:
					return agent.TurnResult{Text: "claude complete", Done: true}, nil
				case <-ctx.Done():
					cancelled <- participant
					return agent.TurnResult{}, ctx.Err()
				}
			}
			<-ctx.Done()
			cancelled <- participant
			return agent.TurnResult{}, ctx.Err()
		}}
	}
	orchestrator, err := New(roomState, nil, roomStore, blocking(chat.Codex), blocking(chat.Claude))
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex inspect parser"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@claude inspect store"); err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	if err := orchestrator.StopScoped("@codex"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cancelled:
		if got != chat.Codex {
			t.Fatalf("cancelled=%s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selected workflow was not cancelled")
	}
	select {
	case got := <-cancelled:
		t.Fatalf("unselected workflow was cancelled: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseClaude)
	orchestrator.wg.Wait()
}

func TestQueuedInputSurvivesRestartAndResumes(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	blocking := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}}
	first, err := New(roomState, nil, roomStore, blocking)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Post("@codex active before restart"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := first.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := first.Post("@codex durable queued input"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := roomStore.LoadMessages(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.PendingInputs) != 1 || loaded.WorkflowMode != chat.WorkflowPlan || messages[len(messages)-1].WorkflowMode != chat.WorkflowPlan {
		t.Fatalf("persisted queue mode: room=%q pending=%v messages=%+v", loaded.WorkflowMode, loaded.PendingInputs, messages)
	}
	resumedRequest := make(chan agent.TurnRequest, 1)
	restartedAgent := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		resumedRequest <- request
		return agent.TurnResult{Text: "resumed queued input", Done: true}, nil
	}}
	restarted, err := New(loaded, messages, roomStore, restartedAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Configure(nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-resumedRequest:
		if !strings.Contains(request.Prompt, "durable queued input") || !strings.Contains(request.Prompt, "PLAN ONLY") || request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 {
			t.Fatalf("resumed plan request=%+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persisted queue did not resume")
	}
	restarted.wg.Wait()
	if got := restarted.PendingInputCount(); got != 0 {
		t.Fatalf("pending count after resume=%d", got)
	}
}

func TestPendingInputIsHiddenFromActiveTurnAndCapsSessionCursor(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.mu.Lock()
	orchestrator.messages = []chat.Message{
		{Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "active request"},
		{Sequence: 2, Author: chat.User, Kind: chat.MessageText, Text: "queued future request"},
		{Sequence: 3, Author: chat.Codex, Kind: chat.MessageText, Text: "current workflow output"},
	}
	orchestrator.room.PendingInputs = []uint64{2}
	orchestrator.mu.Unlock()
	request := orchestrator.turnRequest(chat.Codex, turnSpec{after: 0, through: 3}, nil)
	if strings.Contains(request.Prompt, "queued future request") || !strings.Contains(request.Prompt, "current workflow output") {
		t.Fatalf("pending visibility prompt=%s", request.Prompt)
	}
	if got := orchestrator.seenThrough(3); got != 1 {
		t.Fatalf("seenThrough=%d want=1", got)
	}
}

func TestQueuedInputsWithDifferentTargetsRunAsSeparateBatches(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	codexStarted := make(chan int, 2)
	codexSecondPrompt := make(chan string, 1)
	claudePrompt := make(chan string, 1)
	codex := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		codexStarted <- call
		if call == 1 {
			select {
			case <-release:
				return agent.TurnResult{Text: "initial done", Done: true}, nil
			case <-ctx.Done():
				return agent.TurnResult{}, ctx.Err()
			}
		}
		codexSecondPrompt <- request.Prompt
		return agent.TurnResult{Text: "codex batch done", Done: true}, nil
	}}
	claude := &fakeAgent{participant: chat.Claude, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		claudePrompt <- request.Prompt
		return agent.TurnResult{Text: "claude batch done", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, claude)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex active"); err != nil {
		t.Fatal(err)
	}
	<-codexStarted
	if err := orchestrator.Post("@codex codex queued batch"); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@claude claude queued batch"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case call := <-codexStarted:
		if call != 2 {
			t.Fatalf("codex call=%d", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("codex batch did not start")
	}
	if prompt := <-codexSecondPrompt; !strings.Contains(prompt, "codex queued batch") || strings.Contains(prompt, "claude queued batch") {
		t.Fatalf("codex batch prompt=%s", prompt)
	}
	select {
	case prompt := <-claudePrompt:
		if !strings.Contains(prompt, "claude queued batch") {
			t.Fatalf("claude batch prompt=%s", prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claude batch did not start")
	}
	orchestrator.wg.Wait()
	if got := orchestrator.PendingInputCount(); got != 0 {
		t.Fatalf("pending count=%d", got)
	}
}

func TestPlanModeIsPersistedStampedAndPreservedAcrossQueuedBoundaries(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan agent.TurnRequest, 2)
	releaseFirst := make(chan struct{})
	codex := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		requests <- request
		if call == 1 {
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return agent.TurnResult{}, ctx.Err()
			}
		}
		return agent.TurnResult{Text: fmt.Sprintf("result %d", call), SessionID: fmt.Sprintf("session-%d", call), Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil || loaded.WorkflowMode != chat.WorkflowPlan {
		t.Fatalf("persisted mode=%q err=%v", loaded.WorkflowMode, err)
	}
	if err := orchestrator.Post("@codex inspect the design"); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if first.Settings.Permissions != chat.PermissionReadOnly || len(first.WriteRoots) != 0 || first.VoiceOnly || !strings.Contains(first.Prompt, "PLAN ONLY") || !strings.Contains(first.SystemPrompt, planOnlyInstruction) {
		t.Fatalf("plan request was not host-enforced: %+v", first)
	}

	if err := orchestrator.SetWorkflowMode(chat.WorkflowExecute); err != nil {
		t.Fatal(err)
	}
	if !orchestrator.HasActiveWork() {
		t.Fatal("changing workflow mode interrupted active plan work")
	}
	if err := orchestrator.Post("@codex implement later"); err != nil {
		t.Fatal(err)
	}
	roomCopy, messages := orchestrator.Snapshot()
	if roomCopy.WorkflowMode != chat.WorkflowExecute || len(roomCopy.PendingInputs) != 1 || len(messages) != 2 || messages[0].WorkflowMode != chat.WorkflowPlan || messages[1].WorkflowMode != chat.WorkflowExecute {
		t.Fatalf("mode-stamped queue state: room=%+v messages=%+v", roomCopy, messages)
	}

	close(releaseFirst)
	select {
	case second := <-requests:
		if second.Settings.Permissions != chat.PermissionWorkspace || strings.Contains(second.Prompt, "HOST-ENFORCED TURN MODE: PLAN ONLY") {
			t.Fatalf("queued execute request inherited plan mode: %+v", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued execute request did not start at the safe boundary")
	}
	orchestrator.wg.Wait()
	if codex.callCount() != 2 || orchestrator.PendingInputCount() != 0 {
		t.Fatalf("calls=%d pending=%d", codex.callCount(), orchestrator.PendingInputCount())
	}
}

func TestPlanModeRejectsAccessExpansionWithoutRetry(t *testing.T) {
	orchestrator, codex, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Fatalf("plan permission=%q", request.Settings.Permissions)
		}
		return agent.TurnResult{
			Text: "plan with an unavailable input", Done: true,
			AccessRequest: &agent.AccessRequest{Path: "../outside", Mode: chat.AccessReadWrite, Reason: "not allowed in plan mode"},
		}, nil
	}
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex create a plan"); err != nil {
		t.Fatal(err)
	}
	warningSeen := false
	waitForRoundAllowError(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventWarning && strings.Contains(event.Text, "plan mode cannot expand permissions") {
			warningSeen = true
		}
	})
	roomCopy, _ := orchestrator.Snapshot()
	if !warningSeen || codex.callCount() != 1 || len(roomCopy.Grants) != 1 {
		t.Fatalf("warning=%v calls=%d grants=%+v", warningSeen, codex.callCount(), roomCopy.Grants)
	}
}

func TestSetWorkflowModeRejectsInvalidAndRollsBackFailedPersistence(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	controlled := &controlledStore{base: base}
	orchestrator, err := New(roomState, nil, controlled, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	if err := orchestrator.SetWorkflowMode(chat.WorkflowMode("invalid")); err == nil {
		t.Fatal("invalid workflow mode was accepted")
	}
	controlled.failNextSave()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err == nil {
		t.Fatal("workflow mode save unexpectedly succeeded")
	}
	roomCopy, _ := orchestrator.Snapshot()
	loaded, loadErr := base.LoadRoom(roomState.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if roomCopy.WorkflowMode != chat.WorkflowExecute || loaded.WorkflowMode != chat.WorkflowExecute {
		t.Fatalf("failed save leaked mode: memory=%q disk=%q", roomCopy.WorkflowMode, loaded.WorkflowMode)
	}
}

func TestPlanModeRejectsModeratorRosterMutation(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, worker: false}
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		if request.Settings.Permissions != chat.PermissionReadOnly {
			t.Fatalf("plan moderator permission=%q", request.Settings.Permissions)
		}
		return agent.TurnResult{Text: "plan without roster mutation", Done: true, Joins: []chat.Participant{worker}}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex, &fakeAgent{participant: worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("make a plan"); err != nil {
		t.Fatal(err)
	}
	warningSeen := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventWarning && strings.Contains(event.Text, "Plan mode rejected moderator roster changes") {
			warningSeen = true
		}
	})
	roomCopy, _ := orchestrator.Snapshot()
	if !warningSeen || roomCopy.Present(worker) {
		t.Fatalf("warning=%v members=%+v", warningSeen, roomCopy.Members)
	}
}

func TestHostResearchContinuesSameTurnWithoutPublishingIntermediateDraft(t *testing.T) {
	stateRoot := t.TempDir()
	roomStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true}
	roomState.StreamMode = chat.StreamHistory
	worker := &fakeAgent{participant: chat.Codex}
	worker.run = func(_ context.Context, call int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		switch call {
		case 1:
			if !strings.Contains(request.SystemPrompt, "Host-mediated web research") {
				t.Fatalf("research contract missing from system prompt: %s", request.SystemPrompt)
			}
			emit(agent.Event{Type: agent.EventDelta, Text: "intermediate draft that must disappear"})
			return agent.TurnResult{Text: "intermediate draft that must disappear", SessionID: "research-session", Research: []agent.ResearchRequest{{Type: "search", Query: "Go release notes"}}}, nil
		case 2:
			if !strings.Contains(request.Prompt, "HOST-PROVIDED WEB RESEARCH RESULTS") || !strings.Contains(request.Prompt, "https://go.dev/doc/devel/release") {
				t.Fatalf("host results missing from continuation: %s", request.Prompt)
			}
			return agent.TurnResult{Text: "grounded final answer", SessionID: "research-session", Done: true}, nil
		default:
			t.Fatalf("unexpected agent call %d", call)
			return agent.TurnResult{}, nil
		}
	}
	orchestrator, err := New(roomState, nil, roomStore, worker)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	researcher := &fakeResearcher{results: []agent.ResearchResult{{Type: "search", Query: "Go release notes", Hits: []agent.ResearchHit{{Title: "Release History", URL: "https://go.dev/doc/devel/release"}}}}}
	orchestrator.ConfigureResearch(researcher)
	preferences, err := appsettings.Open(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWebSearchEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex research the current release"); err != nil {
		t.Fatal(err)
	}
	resetSeen := false
	var startedTurnID, finishedTurnID string
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventTurnStarted {
			startedTurnID = event.TurnID
		}
		if event.Type == EventAgent && event.AgentEvent != nil && event.AgentEvent.Type == agent.EventReset {
			resetSeen = true
		}
		if event.Type == EventTurnFinished {
			finishedTurnID = event.TurnID
		}
	})
	if researcher.callCount() != 1 || worker.callCount() != 2 || !resetSeen || startedTurnID == "" || finishedTurnID != startedTurnID {
		t.Fatalf("research calls=%d agent calls=%d reset=%v started=%q finished=%q", researcher.callCount(), worker.callCount(), resetSeen, startedTurnID, finishedTurnID)
	}
	roomCopy, messages := orchestrator.Snapshot()
	if roomCopy.Sessions[chat.Codex].ID != "research-session" {
		t.Fatalf("research continuation session not saved: %+v", roomCopy.Sessions[chat.Codex])
	}
	if len(roomCopy.TurnHistory) != 1 {
		t.Fatalf("turn history=%+v", roomCopy.TurnHistory)
	}
	record := roomCopy.TurnHistory[0]
	if record.State != chat.TurnRecordFinal || record.FinalSequence == 0 || len(record.Drafts) != 1 || record.Drafts[0] != "intermediate draft that must disappear" || len(record.Tools) == 0 || !strings.Contains(record.Tools[0], "host web research batch") {
		t.Fatalf("turn record=%+v", record)
	}
	persisted, err := roomStore.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.TurnHistory) != 1 || persisted.TurnHistory[0].ID != record.ID {
		t.Fatalf("persisted turn history=%+v", persisted.TurnHistory)
	}
	var finalSeen, toolSeen bool
	for _, message := range messages {
		if strings.Contains(message.Text, "intermediate draft") {
			t.Fatalf("intermediate research request leaked into transcript: %+v", message)
		}
		finalSeen = finalSeen || message.Text == "grounded final answer"
		toolSeen = toolSeen || (message.Kind == chat.MessageTool && strings.Contains(message.Text, "host web research batch"))
		if message.Author == chat.Codex && message.TurnID != startedTurnID {
			t.Fatalf("message turn id=%q want %q: %+v", message.TurnID, startedTurnID, message)
		}
	}
	if !finalSeen || !toolSeen {
		t.Fatalf("final=%v tool=%v messages=%+v", finalSeen, toolSeen, messages)
	}
}

func TestTurnCaptureStripsPrivateMarkersAndBoundsContent(t *testing.T) {
	capture := &turnCapture{}
	capture.addDelta("visible draft\n<!-- mohuddle:{\"done\":true} -->")
	capture.addTool("visible tool <!-- mohuddle:{\"done\":true} --> literal")
	capture.addTool(strings.Repeat("x", 8*1024))
	drafts, tools := capture.snapshot()
	if len(drafts) != 1 || drafts[0] != "visible draft" || strings.Contains(strings.Join(drafts, ""), "mohuddle") {
		t.Fatalf("drafts=%q", drafts)
	}
	if len(tools) != 2 || tools[0] != "visible tool <!-- mohuddle:{\"done\":true} --> literal" || len(tools[1]) > 4*1024 {
		t.Fatalf("tools=%d bytes=%d", len(tools), len(tools[0]))
	}
}

func TestTurnHistoryIsBoundedByCountAndBytes(t *testing.T) {
	roomState := chat.Room{}
	for index := 0; index < maxTurnHistoryRecords+5; index++ {
		roomState.TurnHistory = append(roomState.TurnHistory, chat.TurnRecord{ID: fmt.Sprintf("turn-%02d", index), Drafts: []string{"draft"}})
	}
	trimTurnHistoryLocked(&roomState)
	if len(roomState.TurnHistory) != maxTurnHistoryRecords || roomState.TurnHistory[0].ID != "turn-05" {
		t.Fatalf("count-bounded history=%d first=%q", len(roomState.TurnHistory), roomState.TurnHistory[0].ID)
	}

	roomState.TurnHistory = nil
	for index := 0; index < 6; index++ {
		roomState.TurnHistory = append(roomState.TurnHistory, chat.TurnRecord{ID: fmt.Sprintf("large-%d", index), Drafts: []string{strings.Repeat("x", 100*1024)}})
	}
	trimTurnHistoryLocked(&roomState)
	if len(roomState.TurnHistory) >= 6 {
		t.Fatalf("byte-bounded history retained %d records", len(roomState.TurnHistory))
	}
	total := 0
	for _, record := range roomState.TurnHistory {
		total += turnRecordSize(record)
	}
	if total > maxTurnHistoryBytes {
		t.Fatalf("byte-bounded history size=%d", total)
	}
}

func TestCompleteTurnCaptureFiltersOnlyContentlessInterruptions(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := base.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.StreamMode = chat.StreamHistory
	orchestrator, err := New(roomState, nil, base, &fakeAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	startedAt := time.Now().UTC()
	interrupted := turnOutcome{participant: chat.Codex, failed: true}

	for index, value := range []string{"", " \n\t ", `<!-- mohuddle:{"done":true} -->`} {
		capture := &turnCapture{}
		capture.addDelta(value)
		if record := orchestrator.completeTurnCapture(fmt.Sprintf("empty-%d", index), chat.Codex, "lead", "test", startedAt, interrupted, capture); record != nil {
			t.Fatalf("contentless interruption produced record: %+v", record)
		}
	}

	draftCapture := &turnCapture{}
	draftCapture.addDelta("useful partial response")
	draftRecord := orchestrator.completeTurnCapture("draft", chat.Codex, "lead", "test", startedAt, interrupted, draftCapture)
	if draftRecord == nil || len(draftRecord.Drafts) != 1 {
		t.Fatalf("draft-only interruption=%+v", draftRecord)
	}

	toolCapture := &turnCapture{}
	toolCapture.addTool("go test ./...")
	toolRecord := orchestrator.completeTurnCapture("tool", chat.Codex, "lead", "test", startedAt, interrupted, toolCapture)
	if toolRecord == nil || len(toolRecord.Tools) != 1 {
		t.Fatalf("tool-only interruption=%+v", toolRecord)
	}

	published := interrupted
	published.response = 7
	publishedRecord := orchestrator.completeTurnCapture("published", chat.Codex, "lead", "test", startedAt, published, &turnCapture{})
	if publishedRecord == nil || publishedRecord.State != chat.TurnRecordInterrupted || publishedRecord.FinalSequence != 7 {
		t.Fatalf("sequence-only interruption=%+v", publishedRecord)
	}

	silentRecord := orchestrator.completeTurnCapture("silent", chat.Codex, "reviewer", "test", startedAt, turnOutcome{participant: chat.Codex}, &turnCapture{})
	if silentRecord == nil || silentRecord.State != chat.TurnRecordSilent {
		t.Fatalf("silent history record=%+v", silentRecord)
	}

	before, _ := orchestrator.Snapshot()
	for index := 0; index < maxTurnHistoryRecords*2; index++ {
		if record := orchestrator.completeTurnCapture(fmt.Sprintf("blank-%d", index), chat.Codex, "lead", "test", startedAt, interrupted, &turnCapture{}); record != nil {
			t.Fatalf("blank failure %d produced record: %+v", index, record)
		}
	}
	after, _ := orchestrator.Snapshot()
	if len(after.TurnHistory) != len(before.TurnHistory) || len(after.TurnHistory) != 4 {
		t.Fatalf("blank failures changed useful history: before=%d after=%d records=%+v", len(before.TurnHistory), len(after.TurnHistory), after.TurnHistory)
	}
}

func TestDisabledResearchFailsClosedThroughHostContinuation(t *testing.T) {
	stateRoot := t.TempDir()
	roomStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true}
	worker := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			if strings.Contains(request.SystemPrompt, "Host-mediated web research") {
				t.Fatal("disabled research contract was exposed")
			}
			return agent.TurnResult{Research: []agent.ResearchRequest{{Type: "open", URL: "https://example.com"}}}, nil
		}
		if !strings.Contains(request.Prompt, "host web research is disabled") {
			t.Fatalf("disabled result missing: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "answered without web", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, worker)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	researcher := &fakeResearcher{}
	orchestrator.ConfigureResearch(researcher)
	preferences, err := appsettings.Open(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex answer locally"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	if researcher.callCount() != 0 || worker.callCount() != 2 {
		t.Fatalf("disabled broker calls=%d agent calls=%d", researcher.callCount(), worker.callCount())
	}
}

func TestPlanProposalSurvivesRestartAndExecutesExactPlanInFreshContext(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	const planContent = "# Exact accepted plan\n\n1. Re-read the relevant files.\n2. Implement the guarded change.\n3. Run the tests."
	var planLeadCalls atomic.Int32
	var planReviewCalls atomic.Int32
	var acceptedMu sync.Mutex
	var acceptedRequests []agent.TurnRequest
	makeRun := func(participant chat.Participant) func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			if request.Ephemeral {
				return bidResult(participant, chat.Codex), nil
			}
			if strings.Contains(request.Prompt, "PLAN ONLY") {
				if participant == chat.Codex {
					planLeadCalls.Add(1)
					return agent.TurnResult{
						Text:      "The design is ready.\n\n<proposed_plan>\n" + planContent + "\n</proposed_plan>",
						SessionID: "planning-codex", Done: true,
					}, nil
				}
				planReviewCalls.Add(1)
				return agent.TurnResult{SessionID: "planning-claude", Done: true}, nil
			}
			if strings.Contains(request.SystemPrompt, "<accepted_plan") {
				acceptedMu.Lock()
				acceptedRequests = append(acceptedRequests, request)
				acceptedMu.Unlock()
				if request.Settings.Permissions == chat.PermissionReadOnly {
					return agent.TurnResult{Done: true}, nil
				}
				return agent.TurnResult{Text: "implemented exact accepted plan", SessionID: "execution-" + string(participant), Done: true}, nil
			}
			return agent.TurnResult{Text: string(participant) + " unexpected", Done: true}, nil
		}
	}
	codex := &fakeAgent{participant: chat.Codex, run: makeRun(chat.Codex)}
	claude := &fakeAgent{participant: chat.Claude, run: makeRun(chat.Claude)}
	orchestrator, err := New(roomState, nil, roomStore, codex, claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("design the guarded change"); err != nil {
		t.Fatal(err)
	}
	planReady := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		if event.Type == EventPlanReady {
			planReady = event.Plan != nil && event.Plan.Valid() && event.Plan.Content == planContent
		}
	})
	orchestrator.wg.Wait()
	roomBeforeRestart, messagesBeforeRestart := orchestrator.Snapshot()
	if !planReady || roomBeforeRestart.PendingPlan == nil || !roomBeforeRestart.PendingPlan.Valid() || roomBeforeRestart.PendingPlan.Content != planContent {
		t.Fatalf("proposal was not persisted: ready=%v room=%+v", planReady, roomBeforeRestart.PendingPlan)
	}
	if planLeadCalls.Load() != 1 || planReviewCalls.Load() != 1 {
		t.Fatalf("plan calls lead=%d review=%d; silent review should add no closing turn", planLeadCalls.Load(), planReviewCalls.Load())
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
	if loadedRoom.PendingPlan == nil || !loadedRoom.PendingPlan.Valid() || loadedRoom.PendingPlan.Content != planContent || len(loadedMessages) != len(messagesBeforeRestart) {
		t.Fatalf("restart state lost proposal: room=%+v messages=%d want=%d", loadedRoom.PendingPlan, len(loadedMessages), len(messagesBeforeRestart))
	}
	restarted, err := New(loadedRoom, loadedMessages, roomStore, codex, claude)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ExecutePendingPlan(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ExecutePendingPlan(); err == nil {
		t.Fatal("duplicate plan approval was accepted")
	}
	waitForRound(t, restarted.Events(), nil)
	restarted.wg.Wait()

	acceptedMu.Lock()
	requests := append([]agent.TurnRequest(nil), acceptedRequests...)
	acceptedMu.Unlock()
	if len(requests) == 0 {
		t.Fatal("execution received no accepted-plan request")
	}
	workspaceRequestFound := false
	for _, request := range requests {
		if !strings.Contains(request.SystemPrompt, planContent) || !strings.Contains(request.SystemPrompt, chat.ProposedPlanHash(planContent)) {
			t.Fatalf("execution request lost exact plan: %s", request.SystemPrompt)
		}
		if request.Settings.Permissions == chat.PermissionWorkspace {
			workspaceRequestFound = true
			if strings.Contains(request.Prompt, "design the guarded change") {
				t.Fatalf("fresh execution context replayed planning request: %s", request.Prompt)
			}
		}
	}
	if !workspaceRequestFound {
		t.Fatal("accepted plan never reached a writable lead")
	}
	if codex.resetCount() != 0 || claude.resetCount() != 0 {
		t.Fatalf("accepted plan reset unrelated provider sessions: codex=%d claude=%d", codex.resetCount(), claude.resetCount())
	}
	roomAfter, messagesAfter := restarted.Snapshot()
	if roomAfter.WorkflowMode != chat.WorkflowExecute || roomAfter.PendingPlan != nil {
		t.Fatalf("accepted plan state=%+v", roomAfter)
	}
	acceptedMessageFound := false
	for _, message := range messagesAfter {
		if message.AcceptedPlan != nil {
			acceptedMessageFound = message.Author == chat.User && message.WorkflowMode == chat.WorkflowExecute && message.AcceptedPlan.Valid() && message.AcceptedPlan.Content == planContent
		}
	}
	if !acceptedMessageFound || len(messagesBeforeRestart) == 0 {
		t.Fatalf("accepted plan message missing: %+v", messagesAfter)
	}
}

func TestMaterialPlanReviewReturnsOnceToSelectedLead(t *testing.T) {
	orchestrator, codex, claude := newTestOrchestrator(t)
	defer orchestrator.Close()
	var leadCalls atomic.Int32
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		call := leadCalls.Add(1)
		if call == 1 {
			return agent.TurnResult{Text: "<proposed_plan>\n# Draft\n\n- Implement\n</proposed_plan>", Done: true}, nil
		}
		if !strings.Contains(request.Prompt, "must include validation") {
			t.Fatalf("lead synthesis did not receive review: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "<proposed_plan>\n# Final\n\n- Implement\n- Validate\n</proposed_plan>", Done: true}, nil
	}
	claude.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Codex), nil
		}
		return agent.TurnResult{Text: "The plan must include validation.", Done: true}, nil
	}
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("prepare a reviewed plan"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
	orchestrator.wg.Wait()
	roomCopy, _ := orchestrator.Snapshot()
	if leadCalls.Load() != 2 || claude.callCount() != 2 || roomCopy.PendingPlan == nil || roomCopy.PendingPlan.Content != "# Final\n\n- Implement\n- Validate" {
		t.Fatalf("review flow lead_calls=%d claude_calls=%d pending=%+v", leadCalls.Load(), claude.callCount(), roomCopy.PendingPlan)
	}
}

func TestDirectPlanTurnAlsoProducesPendingDecision(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if !strings.Contains(request.Prompt, "PLAN ONLY") {
			t.Fatalf("direct plan request was not enforced: %s", request.Prompt)
		}
		return agent.TurnResult{Text: "<proposed_plan>\n# Direct plan\n\n- Inspect first\n</proposed_plan>", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex plan this directly"); err != nil {
		t.Fatal(err)
	}
	ready := false
	waitForRound(t, orchestrator.Events(), func(event Event) {
		ready = ready || event.Type == EventPlanReady
	})
	orchestrator.wg.Wait()
	roomCopy, _ := orchestrator.Snapshot()
	if !ready || roomCopy.PendingPlan == nil || roomCopy.PendingPlan.Content != "# Direct plan\n\n- Inspect first" {
		t.Fatalf("direct proposal ready=%v pending=%+v", ready, roomCopy.PendingPlan)
	}
}

func TestPlanMayBeApprovedImmediatelyFromPlanReadyEvent(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	codex := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		if strings.Contains(request.Prompt, "PLAN ONLY") {
			return agent.TurnResult{Text: "<proposed_plan>\n# Immediate plan\n\n- Execute safely\n</proposed_plan>", Done: true}, nil
		}
		if !strings.Contains(request.SystemPrompt, "# Immediate plan") {
			t.Fatalf("execution lost accepted plan: %s", request.SystemPrompt)
		}
		return agent.TurnResult{Text: "executed", Done: true}, nil
	}}
	orchestrator, err := New(roomState, nil, roomStore, codex)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex prepare immediate approval"); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(4 * time.Second)
	approved := false
	rounds := 0
	for rounds < 2 {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventError {
				t.Fatalf("orchestrator error: %v", event.Err)
			}
			if event.Type == EventPlanReady && !approved {
				if err := orchestrator.ExecutePendingPlan(); err != nil {
					t.Fatalf("approval from plan-ready event failed: %v", err)
				}
				approved = true
			}
			if event.Type == EventRoundDone {
				rounds++
			}
		case <-timeout:
			t.Fatalf("timed out: approved=%v rounds=%d", approved, rounds)
		}
	}
	orchestrator.wg.Wait()
	roomCopy, _ := orchestrator.Snapshot()
	if !approved || roomCopy.PendingPlan != nil || roomCopy.WorkflowMode != chat.WorkflowExecute {
		t.Fatalf("immediate approval state: approved=%v room=%+v", approved, roomCopy)
	}
}

func TestStopClearsQueuedInput(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.mu.Lock()
	orchestrator.room.PendingInputs = []uint64{10, 11}
	orchestrator.mu.Unlock()
	orchestrator.Stop()
	if got := orchestrator.PendingInputCount(); got != 0 {
		t.Fatalf("pending count after stop=%d", got)
	}
}

func TestQueuedInputSchedulesConfirmedAvailabilityRetry(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	now := time.Now()
	retryAt := now.Add(2 * time.Minute)
	orchestrator.mu.Lock()
	orchestrator.room.PendingInputs = []uint64{10}
	orchestrator.room.Availability[chat.Codex] = chat.ParticipantAvailability{RetryAt: &retryAt}
	orchestrator.mu.Unlock()
	delay, scheduled := orchestrator.nextRosterActionDelay(now)
	if !scheduled || delay < 119*time.Second || delay > 121*time.Second {
		t.Fatalf("retry scheduled=%v delay=%s", scheduled, delay)
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
	if err := orchestrator.Steer("@codex steering that cannot persist"); err == nil {
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
	if err := orchestrator.Steer("@codex persisted message with failed room snapshot"); err == nil {
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
	moderatorOutcome := func(o *Orchestrator, result agent.TurnResult, version uint64) turnOutcome {
		return turnOutcome{participant: chat.Codex, result: result, authority: turnAuthority{
			participant: chat.Codex, workflowVersion: version, role: "moderator",
			mayDelegate: true, mayManageRoster: true, policy: chat.DelegationAuto,
		}}
	}

	t.Run("join plus invalid delegate changes nothing", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		_, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: chat.Codex, Task: "invalid"}}}, o.version), true, true)
		if err == nil || snapshotRoom(o).Present(worker) {
			t.Fatalf("invalid combined marker was partially applied: err=%v members=%+v", err, snapshotRoom(o).Members)
		}
	})

	t.Run("join plus delegate uses projected membership", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		plan, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, o.version), true, true)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshotRoom(o).Present(worker) || len(plan.reserved) != 1 {
			t.Fatalf("valid projected marker was not prepared: plan=%+v room=%+v", plan, snapshotRoom(o).Members)
		}
		o.releaseTurnControlPlan(plan, false)
	})

	t.Run("leave plus delegate is rejected", func(t *testing.T) {
		o, worker := newOrchestrator(t, true, nil)
		defer o.Close()
		_, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Leaves: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, o.version), true, true)
		if err == nil || !snapshotRoom(o).Present(worker) {
			t.Fatalf("leave/delegate conflict was partially applied: err=%v room=%+v", err, snapshotRoom(o).Members)
		}
	})

	t.Run("stale and repeated wave controls do not mutate roster", func(t *testing.T) {
		o, worker := newOrchestrator(t, false, nil)
		defer o.Close()
		version := o.version
		o.Stop()
		if _, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Joins: []chat.Participant{worker}}, version), false, false); !errors.Is(err, errWorkflowSuperseded) {
			t.Fatalf("stale control error=%v", err)
		}
		o.mu.Lock()
		o.delegationStates[o.version] = &workflowDelegationState{version: o.version, boundaries: maxDelegationBoundaries, used: make(map[chat.Participant]bool)}
		o.mu.Unlock()
		if _, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "again"}}}, o.version), true, true); err == nil {
			t.Fatal("exhausted delegation boundary unexpectedly succeeded")
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
		_, err := o.prepareTurnControls(moderatorOutcome(o, agent.TurnResult{Joins: []chat.Participant{worker}, Delegates: []agent.DelegationRequest{{Participant: worker, Task: "inspect"}}}, o.version), true, true)
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

func TestDelegationSaturationPinsAskTargetsAndFallsBackUnderAuto(t *testing.T) {
	newSaturatedOrchestrator := func(t *testing.T) (*Orchestrator, chat.Participant) {
		t.Helper()
		roomStore, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		roomState, err := roomStore.Create(t.TempDir(), 3)
		if err != nil {
			t.Fatal(err)
		}
		codexOne, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
		codexTwo, _ := chat.AuxiliaryParticipant(chat.Codex, 2)
		roomState.Members = map[chat.Participant]bool{
			chat.Codex: true, chat.Claude: true, chat.Agy: true, codexOne: true, codexTwo: true,
		}
		orchestrator, err := New(roomState, nil, roomStore,
			&fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude}, &fakeAgent{participant: chat.Agy},
			&fakeAgent{participant: codexOne}, &fakeAgent{participant: codexTwo},
		)
		if err != nil {
			t.Fatal(err)
		}
		orchestrator.mu.Lock()
		_, primaryCancel := context.WithCancel(context.Background())
		_, auxiliaryCancel := context.WithCancel(context.Background())
		orchestrator.activeTurns[chat.Codex] = activeTurn{cancel: primaryCancel}
		orchestrator.activeTurns[codexTwo] = activeTurn{cancel: auxiliaryCancel}
		orchestrator.mu.Unlock()
		return orchestrator, codexOne
	}
	proposal := func(target chat.Participant, policy chat.DelegationPolicy) turnOutcome {
		return turnOutcome{participant: chat.Claude, result: agent.TurnResult{Delegates: []agent.DelegationRequest{{Participant: target, Task: "inspect logs"}}}, authority: turnAuthority{
			participant: chat.Claude, workflowVersion: 0, role: "lead", mayDelegate: true, policy: policy,
		}}
	}

	t.Run("Ask keeps approved identity queued", func(t *testing.T) {
		orchestrator, target := newSaturatedOrchestrator(t)
		defer orchestrator.Close()
		plan, err := orchestrator.prepareTurnControls(proposal(target, chat.DelegationAsk), true, true)
		if err != nil {
			t.Fatal(err)
		}
		defer orchestrator.releaseTurnControlPlan(plan, false)
		if len(plan.delegates) != 1 || plan.delegates[0].Participant != target || len(plan.reassignments) != 0 {
			t.Fatalf("pinned plan=%+v", plan)
		}
	})

	t.Run("Auto selects an available fallback", func(t *testing.T) {
		orchestrator, target := newSaturatedOrchestrator(t)
		defer orchestrator.Close()
		plan, err := orchestrator.prepareTurnControls(proposal(target, chat.DelegationAuto), true, true)
		if err != nil {
			t.Fatal(err)
		}
		defer orchestrator.releaseTurnControlPlan(plan, false)
		if len(plan.delegates) != 1 || plan.delegates[0].Participant != chat.Agy || len(plan.reassignments) != 1 {
			t.Fatalf("automatic fallback plan=%+v", plan)
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
	worker, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, worker: true}
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

func TestHumanCanDelegateToPresentPrimaryRoomAI(t *testing.T) {
	orchestrator, _, claude := newTestOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.Delegate(chat.Claude, "inspect the parser"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventDelegationDone && event.Participant == chat.Claude {
				if claude.callCount() != 1 {
					t.Fatalf("claude calls=%d", claude.callCount())
				}
				return
			}
		case <-deadline:
			t.Fatal("primary delegation did not complete")
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
	roomState.StreamMode = chat.StreamStable
	started := make(chan struct{}, 1)
	helper := &fakeAgent{participant: worker, run: func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		emit(agent.Event{Type: agent.EventDelta, Agent: worker, Text: "visible interrupted draft"})
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
	if err := orchestrator.Delegate(worker, "overlap"); err == nil || !(strings.Contains(err.Error(), "already working") || strings.Contains(err.Error(), "delegated work")) {
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
	roomCopy, messages := orchestrator.Snapshot()
	if len(roomCopy.TurnHistory) != 1 || roomCopy.TurnHistory[0].State != chat.TurnRecordInterrupted || len(roomCopy.TurnHistory[0].Drafts) != 1 || roomCopy.TurnHistory[0].Drafts[0] != "visible interrupted draft" {
		t.Fatalf("interrupted turn history=%+v", roomCopy.TurnHistory)
	}
	for _, message := range messages {
		if message.Kind == chat.MessageInterrupted || strings.Contains(message.Text, "visible interrupted draft") {
			t.Fatalf("interrupted draft leaked into final transcript: %+v", message)
		}
	}
	persisted, err := base.LoadRoom(roomState.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.TurnHistory) != 1 || persisted.TurnHistory[0].Drafts[0] != "visible interrupted draft" {
		t.Fatalf("stable-mode interrupted history was not durable: %+v", persisted.TurnHistory)
	}
}

func snapshotRoom(orchestrator *Orchestrator) chat.Room {
	value, _ := orchestrator.Snapshot()
	return value
}

func TestWorkflowActiveTracksRoomWorkButNotConversationRouting(t *testing.T) {
	var absent *Orchestrator
	if absent.WorkflowActive() {
		t.Fatal("nil orchestrator reported active workflow")
	}
	orchestrator := &Orchestrator{room: chat.Room{
		PendingRoutes: []uint64{1},
		Conversations: []chat.ConversationJob{{State: chat.ConversationWaiting}},
	}}
	if orchestrator.WorkflowActive() {
		t.Fatal("routing and read-only conversations counted as active workflow")
	}
	orchestrator.activeWork = 1
	if !orchestrator.WorkflowActive() {
		t.Fatal("room workflow was not reported active")
	}
}

func TestNormalizeConversationInboxMergesDuplicatesAndHidesLegacyFailures(t *testing.T) {
	now := time.Now().UTC()
	earlier := now.Add(-time.Minute)
	deadline := now.Add(time.Minute)
	completed := earlier.Add(time.Second)
	roomState := chat.Room{
		PendingRoutes: []uint64{1, 2, 3, 4, 99},
		Activities: map[chat.Participant]chat.ParticipantActivity{
			chat.Codex: {Participant: chat.Codex, Role: "conversation responder", State: chat.SchedulerActive, Deadline: &deadline},
		},
		Conversations: []chat.ConversationJob{
			{ID: "duplicate", SourceSequence: 1, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, Requested: []chat.Participant{chat.Codex}, CreatedAt: earlier, UpdatedAt: earlier, TerminalReason: "hard response deadline expired", Attempts: []chat.ConversationAttempt{{Participant: chat.Codex, Provider: chat.Codex, StartedAt: earlier, Deadline: deadline, CompletedAt: &completed}}},
			{ID: "duplicate", SourceSequence: 1, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, Requested: []chat.Participant{chat.Claude}, CreatedAt: now, UpdatedAt: now, TerminalReason: "repair marker", Attempts: []chat.ConversationAttempt{{Participant: chat.Claude, Provider: chat.Claude, StartedAt: now, Deadline: deadline}}},
			{ID: "requires-work", SourceSequence: 2, State: chat.ConversationNeedsAttention, Class: chat.ConversationStandard, CreatedAt: now, UpdatedAt: now, TerminalReason: legacyRequiresWorkSentinel},
			{ID: "empty", SourceSequence: 3, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, CreatedAt: now, UpdatedAt: now, TerminalReason: "responder returned no public answer"},
			{ID: "restart", SourceSequence: 4, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, CreatedAt: now, UpdatedAt: now, TerminalReason: "alternate response attempt was interrupted by host restart"},
			// Provider error text mentioning work must never be promoted into a card.
			{ID: "provider-error", SourceSequence: 5, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, CreatedAt: now, UpdatedAt: now, TerminalReason: "provider exited: this asks for work that requires work tooling"},
		},
	}
	changed, notices := normalizeConversationInbox(&roomState, now)
	if !changed {
		t.Fatal("legacy inbox was not normalized")
	}
	if len(notices) != 0 {
		t.Fatalf("legacy reclassification owed no transcript line: %+v", notices)
	}
	if len(roomState.Conversations) != 5 {
		t.Fatalf("duplicate conversations were not merged: %+v", roomState.Conversations)
	}
	merged := roomState.Conversations[0]
	if merged.ID != "duplicate" || merged.State != chat.ConversationFailed || len(merged.Requested) != 2 || len(merged.Attempts) != 2 || merged.Attempts[1].CompletedAt == nil {
		t.Fatalf("merged conversation=%+v", merged)
	}
	if roomState.Conversations[1].ActionState != chat.ConversationRequiresWork || roomState.Conversations[1].DerivedInboxCategory() != chat.ConversationInboxActionNeeded {
		t.Fatalf("requires-work migration=%+v", roomState.Conversations[1])
	}
	for _, job := range roomState.Conversations[2:] {
		if job.State != chat.ConversationFailed || job.DerivedInboxCategory() != chat.ConversationInboxHidden {
			t.Fatalf("legacy failure remained visible: %+v", job)
		}
	}
	if len(roomState.PendingRoutes) != 1 || roomState.PendingRoutes[0] != 99 || roomState.Activities[chat.Codex].State != chat.SchedulerDone {
		t.Fatalf("stale routing/activity survived: routes=%v activity=%+v", roomState.PendingRoutes, roomState.Activities[chat.Codex])
	}
	if again, _ := normalizeConversationInbox(&roomState, now); again {
		t.Fatal("conversation normalization is not idempotent")
	}
}

func TestRequiresWorkConversationRecognizesCurrentAndLegacySentinelsOnly(t *testing.T) {
	if !requiresWorkConversation(legacyRequiresWorkSentinel) {
		t.Fatal("legacy RequiresWork sentinel was not recognized")
	}
	if !requiresWorkConversation(requiresWorkSentinel) {
		t.Fatal("current RequiresWork sentinel was not recognized")
	}
	if requiresWorkConversation("provider exited: this message needs Work tooling") {
		t.Fatal("arbitrary provider text was classified as RequiresWork")
	}
}

func TestNormalizeConversationInboxReportsInterruptedRepliesOnce(t *testing.T) {
	now := time.Now().UTC()
	started := now.Add(-time.Hour)
	expired := now.Add(-time.Minute)
	roomState := chat.Room{
		Conversations: []chat.ConversationJob{
			{ID: "interrupted", SourceSequence: 1, State: chat.ConversationAnswering, Class: chat.ConversationStandard, CreatedAt: started, UpdatedAt: started, Assigned: chat.Codex},
			{ID: "expired", SourceSequence: 2, State: chat.ConversationWaiting, Class: chat.ConversationStandard, CreatedAt: started, UpdatedAt: started, StartedAt: &started, Deadline: &expired},
			{ID: "already-reported", SourceSequence: 3, State: chat.ConversationRetrying, Class: chat.ConversationStandard, CreatedAt: started, UpdatedAt: started, FailureSequence: 7},
		},
	}
	changed, notices := normalizeConversationInbox(&roomState, now)
	if !changed || len(notices) != 2 {
		t.Fatalf("interrupted replies owed one line each: changed=%v notices=%+v", changed, notices)
	}
	if notices[0].id != "interrupted" || notices[1].id != "expired" {
		t.Fatalf("unexpected notice targets: %+v", notices)
	}
	for _, job := range roomState.Conversations {
		if job.State != chat.ConversationFailed || job.DerivedInboxCategory() != chat.ConversationInboxHidden {
			t.Fatalf("interrupted reply remained visible: %+v", job)
		}
	}
	// A record that already carries its failure line must not be reported twice.
	roomState.Conversations[0].FailureSequence = 11
	roomState.Conversations[1].FailureSequence = 12
	roomState.Conversations[0].State = chat.ConversationAnswering
	roomState.Conversations[1].State = chat.ConversationRetrying
	_, repeat := normalizeConversationInbox(&roomState, now)
	if len(repeat) != 0 {
		t.Fatalf("failure line was duplicated across restarts: %+v", repeat)
	}
}

func TestNewAppendsInterruptedReplyTranscriptLine(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour)
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	source := chat.Message{Sequence: 1, ID: "seed-source", Author: chat.User, Kind: chat.MessageText, ConversationID: "interrupted", Text: "how does routing work?", CreatedAt: started}
	if err := roomStore.AppendMessage(roomState.ID, source); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	roomState.Conversations = []chat.ConversationJob{
		{ID: "interrupted", SourceSequence: 1, State: chat.ConversationAnswering, Class: chat.ConversationStandard, CreatedAt: started, UpdatedAt: started, Assigned: chat.Codex},
	}
	if err := roomStore.SaveRoom(cloneRoom(roomState)); err != nil {
		t.Fatalf("save room: %v", err)
	}
	messages := []chat.Message{source}

	orchestrator, err := New(roomState, messages, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	defer orchestrator.Close()

	saved, transcript := orchestrator.Snapshot()
	if len(transcript) != 2 {
		t.Fatalf("interrupted reply did not add exactly one line: %+v", transcript)
	}
	line := transcript[1]
	if line.Author != chat.System || line.ConversationID != "interrupted" || !strings.Contains(line.Text, "interrupted") {
		t.Fatalf("unexpected failure line: %+v", line)
	}
	if len(saved.Conversations) != 1 || saved.Conversations[0].State != chat.ConversationFailed || saved.Conversations[0].FailureSequence != line.Sequence {
		t.Fatalf("failure sequence was not tracked: %+v", saved.Conversations)
	}
	if counts := chat.CountConversationInbox(saved.Conversations); counts.NewAnswers+counts.Working+counts.ActionNeeded != 0 {
		t.Fatalf("interrupted reply left a visible card: %+v", counts)
	}
	persisted, err := roomStore.LoadMessages(roomState.ID)
	if err != nil || len(persisted) != 2 {
		t.Fatalf("failure line was not durable: err=%v messages=%+v", err, persisted)
	}

	// Restarting the same durable state must not append a second line.
	restarted, err := New(saved, transcript, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatalf("restart orchestrator: %v", err)
	}
	defer restarted.Close()
	if _, again := restarted.Snapshot(); len(again) != 2 {
		t.Fatalf("restart duplicated the failure line: %+v", again)
	}

	// The line was written but the job save was lost, so FailureSequence is gone
	// while the transcript still carries the notice. Recovery must adopt that
	// line instead of writing a second one.
	lost := cloneRoom(saved)
	lost.Conversations[0].State = chat.ConversationAnswering
	lost.Conversations[0].FailureSequence = 0
	recovered, err := New(lost, transcript, roomStore, &fakeAgent{participant: chat.Codex}, &fakeAgent{participant: chat.Claude})
	if err != nil {
		t.Fatalf("recover orchestrator: %v", err)
	}
	defer recovered.Close()
	recoveredRoom, recoveredTranscript := recovered.Snapshot()
	if len(recoveredTranscript) != 2 {
		t.Fatalf("lost failure sequence duplicated the line: %+v", recoveredTranscript)
	}
	if recoveredRoom.Conversations[0].FailureSequence != line.Sequence {
		t.Fatalf("existing failure line was not adopted: %+v", recoveredRoom.Conversations[0])
	}
}

func TestNaturalConversationIsDurableReadOnlyAndSkipsBidding(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if !request.Ephemeral || request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0 {
			t.Errorf("conversation request was not isolated read-only: %+v", request)
		}
		return agent.TurnResult{Text: "Use /status to see conflict statistics.", Done: true}, nil
	}

	if err := orchestrator.Post("how do I see the conflict stats?"); err != nil {
		t.Fatal(err)
	}
	job := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	roomState, messages := orchestrator.Snapshot()
	if len(roomState.PendingInputs) != 0 || len(roomState.PendingRoutes) != 0 || len(roomState.Conversations) != 1 {
		t.Fatalf("unexpected routing state: %+v", roomState)
	}
	if len(messages) != 2 || messages[0].InputIntent != chat.InputConversation || messages[0].ConversationID != job.ID || messages[1].ConversationID != job.ID {
		t.Fatalf("unexpected conversation transcript: %+v", messages)
	}
	if !job.Unread || job.AnswerSequence != messages[1].Sequence || codexAgent.callCount() != 1 {
		t.Fatalf("unexpected answer lifecycle: job=%+v calls=%d", job, codexAgent.callCount())
	}
}

func TestConversationResponderSeesEarlierRoomConversation(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	orchestrator.mu.Lock()
	orchestrator.room.Members[chat.Claude] = false
	orchestrator.mu.Unlock()

	secondRequest := make(chan agent.TurnRequest, 1)
	codexAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 2 {
			secondRequest <- request
		}
		return agent.TurnResult{Text: fmt.Sprintf("answer %d", call), Done: true}, nil
	}
	if err := orchestrator.Post("what does alpha mean?"); err != nil {
		t.Fatal(err)
	}
	first := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	if err := orchestrator.Post("can you explain your previous answer?"); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-secondRequest:
		if !strings.Contains(request.Prompt, "what does alpha mean?") || !strings.Contains(request.Prompt, "answer 1") {
			t.Fatalf("earlier room conversation missing from responder context: %s", request.Prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second conversation did not run")
	}
	roomState, _ := orchestrator.Snapshot()
	if len(roomState.Conversations) != 2 || roomState.Conversations[0].ID != first.ID {
		t.Fatalf("conversation history=%+v", roomState.Conversations)
	}
}

func TestNaturalRoutingDoesNotDependOnBusyTiming(t *testing.T) {
	orchestrator, agents := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	agents[chat.Codex].run = func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		close(started)
		select {
		case <-release:
			return agent.TurnResult{Text: "implementation complete", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}
	agents[chat.Agy].run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if !request.Ephemeral || request.Settings.Permissions != chat.PermissionReadOnly {
			t.Errorf("side response was not read-only")
		}
		return agent.TurnResult{Text: "The answer is available while work continues.", Done: true}, nil
	}

	if err := orchestrator.Post("@codex implement the requested change"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("main work did not start")
	}
	routingStarted := time.Now()
	if err := orchestrator.Post("how do I see the current status?"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(routingStarted); elapsed >= 2*time.Second {
		t.Fatalf("local routing took %s", elapsed)
	}
	if err := orchestrator.Post("fix the remaining shortcut too"); err != nil {
		t.Fatal(err)
	}
	job := waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	roomState, _ := orchestrator.Snapshot()
	if len(roomState.PendingInputs) != 1 || job.Assigned != chat.Agy {
		t.Fatalf("question/work routing changed while busy: job=%+v pending=%v", job, roomState.PendingInputs)
	}
	close(release)
}

func TestAmbiguousInputRequiresExplicitRoutingAndPreservesMode(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("this deserves another look"); err != nil {
		t.Fatal(err)
	}
	roomState, messages := orchestrator.Snapshot()
	if len(roomState.PendingRoutes) != 1 || len(roomState.PendingInputs) != 0 || messages[0].InputIntent != chat.InputAmbiguous {
		t.Fatalf("ambiguous input was guessed: room=%+v messages=%+v", roomState, messages)
	}
	if err := orchestrator.ResolveInput(roomState.PendingRoutes[0], chat.InputWork, false); err != nil {
		t.Fatal(err)
	}
	roomState, messages = orchestrator.Snapshot()
	resolution := roomState.InputResolutions[messages[0].Sequence]
	if len(roomState.PendingRoutes) != 0 || len(messages) != 1 || !messages[0].WorkflowMode.PlanOnly() || resolution.Intent != chat.InputWork || resolution.WorkflowID == "" {
		t.Fatalf("resolved work lost its stamped plan mode: room=%+v messages=%+v", roomState, messages)
	}
}

func TestResolvedWorkKeepsConversationSourceInAuthoritativePrompt(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	requests := make(chan agent.TurnRequest, 1)
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		requests <- request
		return agent.TurnResult{Text: "done", Done: true}, nil
	}
	const sourceText = "this deserves another careful look"
	if err := orchestrator.Post("@codex " + sourceText); err != nil {
		t.Fatal(err)
	}
	roomState, messages := orchestrator.Snapshot()
	if len(roomState.PendingRoutes) != 1 || len(messages) != 1 || messages[0].ConversationID == "" {
		t.Fatalf("ambiguous source=%+v room=%+v", messages, roomState)
	}
	if err := orchestrator.ResolveInput(roomState.PendingRoutes[0], chat.InputWork, false); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		want := fmt.Sprintf("[%d] %s", messages[0].Sequence, sourceText)
		if !strings.Contains(request.Prompt, "HOST-ENFORCED AUTHORITATIVE WORKFLOW SOURCES") || !strings.Contains(request.Prompt, want) {
			t.Fatalf("resolved Work source missing from prompt: %s", request.Prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolved Work request was not dispatched")
	}
}

func TestMissingWorkflowSourceTriggersRecoveryBeforeProviderCall(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.mu.Lock()
	orchestrator.version++
	version := orchestrator.version
	orchestrator.registerWorkflowLocked("missing-source", version, []uint64{999}, chat.Codex, chat.WorkflowExecute, chat.DelegationManual, chat.WorkflowWorkspaceWrite)
	_, _, cores, _, err := orchestrator.startWorkflowLocked(version)
	orchestrator.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	go orchestrator.runDirectWorkflow(0, chat.Codex, cores, version, chat.WorkflowExecute, chat.DelegationManual)
	deadline := time.Now().Add(2 * time.Second)
	var record chat.WorkflowRecord
	for time.Now().Before(deadline) {
		roomState, _ := orchestrator.Snapshot()
		record = roomState.Workflows["missing-source"]
		if record.RecoveryAttempts == 1 && !orchestrator.WorkflowActive() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if codexAgent.callCount() != 0 || record.RecoveryAttempts != 1 || !strings.Contains(record.RecoveryReason, "task grounding failure") {
		t.Fatalf("provider calls=%d recovery=%+v", codexAgent.callCount(), record)
	}
}

func TestConversationResponderGroundsRequiresWorkInCurrentSource(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	orchestrator.mu.Lock()
	orchestrator.room.Members[chat.Claude] = false
	orchestrator.mu.Unlock()

	codexAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			return agent.TurnResult{Text: "I can explain that implementation request.", Done: true}, nil
		}
		if strings.Contains(request.Prompt, "write the release file") {
			t.Errorf("unrelated older conversation leaked into hello prompt: %s", request.Prompt)
		}
		if !strings.Contains(request.SystemPrompt, "Decide requires_work from source message") || !strings.Contains(request.SystemPrompt, `"hello?"`) {
			t.Errorf("current conversation source was not host-anchored: %s", request.SystemPrompt)
		}
		return agent.TurnResult{Text: "Hello — I’m here.", Done: true}, nil
	}
	if err := orchestrator.Post("what would it take to write the release file?"); err != nil {
		t.Fatal(err)
	}
	waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	if err := orchestrator.Post("hello?"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var job chat.ConversationJob
	for time.Now().Before(deadline) {
		roomState, _ := orchestrator.Snapshot()
		if len(roomState.Conversations) == 2 && roomState.Conversations[1].State == chat.ConversationAnswered {
			job = roomState.Conversations[1]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job.ID == "" {
		t.Fatal("hello conversation did not finish")
	}
	if job.ActionState == chat.ConversationRequiresWork {
		t.Fatalf("hello became a Work decision: %+v", job)
	}
}

func TestWorkflowLoopRecoveryAlternatesOnceThenPauses(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	loop := func(emit func(agent.Event)) agent.TurnResult {
		for range 3 {
			emit(agent.Event{Type: agent.EventTool, Text: "command: rg --files"})
		}
		return agent.TurnResult{Text: "should be discarded", Done: true}
	}
	codexAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		if err := os.WriteFile(filepath.Join(request.Workspace, "partial.txt"), []byte("preserved"), 0o600); err != nil {
			t.Errorf("write partial workspace change: %v", err)
		}
		return loop(emit), nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		if data, err := os.ReadFile(filepath.Join(request.Workspace, "partial.txt")); err != nil || string(data) != "preserved" {
			t.Errorf("recovery lead did not inherit partial workspace change: data=%q err=%v", data, err)
		}
		return loop(emit), nil
	}

	const sourceText = "implement the loop recovery regression"
	if err := orchestrator.Post("@codex " + sourceText); err != nil {
		t.Fatal(err)
	}
	_, messages := orchestrator.Snapshot()
	workflowID := messages[0].WorkflowID
	deadline := time.Now().Add(3 * time.Second)
	var record chat.WorkflowRecord
	for time.Now().Before(deadline) {
		roomState, _ := orchestrator.Snapshot()
		record = roomState.Workflows[workflowID]
		if record.State == chat.WorkflowNeedsAttention && record.RecoveryAttempts == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if record.State != chat.WorkflowNeedsAttention || record.RecoveryAttempts != 2 || record.RecoveryPending {
		t.Fatalf("recovery did not pause after one retry: %+v", record)
	}
	if record.RecoveryTarget != "" || len(record.RecoveryActors) != 2 || record.RecoveryActors[0] != chat.Codex || record.RecoveryActors[1] != chat.Claude {
		t.Fatalf("recovery participants=%+v target=%s", record.RecoveryActors, record.RecoveryTarget)
	}
	if codexAgent.callCount() != 1 || claudeAgent.callCount() != 1 || claudeAgent.resetCount() != 1 {
		t.Fatalf("calls codex=%d claude=%d claude resets=%d", codexAgent.callCount(), claudeAgent.callCount(), claudeAgent.resetCount())
	}
	for _, runner := range []*fakeAgent{codexAgent, claudeAgent} {
		if !strings.Contains(runner.request(0).Prompt, sourceText) {
			t.Fatalf("recovery request lost authoritative source: %s", runner.request(0).Prompt)
		}
	}
	roomState, transcript := orchestrator.Snapshot()
	if len(roomState.PendingInputs) != 0 {
		t.Fatalf("paused recovery left queued input: %v", roomState.PendingInputs)
	}
	diagnostics := 0
	for _, message := range transcript {
		if message.Author == chat.System && strings.Contains(message.Text, "MoHuddle self-diagnosis") {
			diagnostics++
		}
	}
	if diagnostics != 2 {
		t.Fatalf("diagnostic messages=%d transcript=%+v", diagnostics, transcript)
	}
}

func TestWorkflowLoopRecoveryFallsBackToSameLeadWhenNoAlternateExists(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	codexAgent := &fakeAgent{participant: chat.Codex}
	codexAgent.run = func(_ context.Context, call int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			for range 3 {
				emit(agent.Event{Type: agent.EventTool, Text: "command: rg --files"})
			}
			return agent.TurnResult{Text: "discarded", Done: true}, nil
		}
		return agent.TurnResult{Text: "recovered", Done: true}, nil
	}
	orchestrator, err := New(roomState, nil, roomStore, codexAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex implement same-lead fallback"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var record chat.WorkflowRecord
	for time.Now().Before(deadline) {
		roomCopy, messages := orchestrator.Snapshot()
		if len(messages) > 0 {
			record = roomCopy.Workflows[messages[0].WorkflowID]
		}
		if record.State == chat.WorkflowCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if record.State != chat.WorkflowCompleted || record.RecoveryAttempts != 1 || record.RecoveryTarget != chat.Codex || codexAgent.callCount() != 2 || codexAgent.resetCount() != 1 {
		t.Fatalf("same-lead recovery record=%+v calls=%d resets=%d", record, codexAgent.callCount(), codexAgent.resetCount())
	}
}

func TestModeratedLoopStopsPeerReviewBeforeFreshRecovery(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	loopOrBid := func(participant chat.Participant) func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
		return func(_ context.Context, _ int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
			if request.Ephemeral {
				return bidResult(participant, chat.Codex), nil
			}
			for range 3 {
				emit(agent.Event{Type: agent.EventTool, Text: "command: rg --files"})
			}
			return agent.TurnResult{Text: "discarded", Done: true}, nil
		}
	}
	codexAgent.run = loopOrBid(chat.Codex)
	claudeAgent.run = loopOrBid(chat.Claude)
	if err := orchestrator.Post("implement moderated loop recovery"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	var record chat.WorkflowRecord
	for time.Now().Before(deadline) {
		roomState, messages := orchestrator.Snapshot()
		if len(messages) > 0 {
			record = roomState.Workflows[messages[0].WorkflowID]
		}
		if record.State == chat.WorkflowNeedsAttention && record.RecoveryAttempts == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if record.State != chat.WorkflowNeedsAttention || codexAgent.callCount() != 2 || claudeAgent.callCount() != 2 {
		t.Fatalf("moderated recovery record=%+v calls codex=%d claude=%d", record, codexAgent.callCount(), claudeAgent.callCount())
	}
	if codexAgent.request(1).Settings.Permissions == chat.PermissionReadOnly || claudeAgent.request(1).Settings.Permissions == chat.PermissionReadOnly {
		t.Fatal("recovery ran a peer review instead of a fresh writable lead")
	}
}

func TestLoopDetectorResetsAfterDurableProgressAndIgnoresDistinctActions(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	for index, action := range []string{"command: rg a", "command: rg a", "file change: parser.go", "command: rg a", "command: rg a", "command: rg b", "command: rg c"} {
		if reason := orchestrator.observeWorkflowTool("workflow", "turn", action); reason != "" {
			t.Fatalf("action %d produced a false loop: %s", index, reason)
		}
	}
	var reason string
	for _, action := range []string{"command: rg a", "command: rg b", "command: rg a", "command: rg b", "command: rg a", "command: rg b"} {
		reason = orchestrator.observeWorkflowTool("workflow", "cycle", action)
	}
	if !strings.Contains(reason, "cycle of 2 steps") {
		t.Fatalf("repeated cycle was not detected: %q", reason)
	}
}

func TestRemoteWorkDirectiveRequiresTrustedRoutingInsteadOfChatCompletion(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	route := chat.RouteMetadata{MessageID: "remote-work-1", OriginInstanceID: "phone", OriginClientID: "device", Hops: []string{"gateway"}}
	if err := orchestrator.AskExternal("implement the requested fix", route); err != nil {
		t.Fatal(err)
	}
	roomState, messages := orchestrator.Snapshot()
	if len(roomState.PendingRoutes) != 1 || len(roomState.PendingInputs) != 0 || len(roomState.Conversations) != 0 || len(messages) != 1 {
		t.Fatalf("remote work bypassed trusted routing: room=%+v messages=%+v", roomState, messages)
	}
	if messages[0].InputIntent != chat.InputWork || messages[0].IntentConfidence != chat.IntentHigh || messages[0].Route == nil || messages[0].Route.MessageID != route.MessageID {
		t.Fatalf("remote work classification=%+v", messages[0])
	}
}

func TestMainWorkflowPromptExcludesUnrelatedConversation(t *testing.T) {
	orchestrator, codexAgent, claudeAgent := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	codexAgent.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if call == 1 {
			return agent.TurnResult{Text: "private side answer", Done: true}, nil
		}
		if strings.Contains(request.Prompt, "private side question") || strings.Contains(request.Prompt, "private side answer") {
			t.Errorf("main workflow received unrelated conversation: %s", request.Prompt)
		}
		return bidResult(chat.Codex, chat.Claude), nil
	}
	claudeAgent.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if strings.Contains(request.Prompt, "private side question") || strings.Contains(request.Prompt, "private side answer") {
			t.Errorf("main workflow received unrelated conversation: %s", request.Prompt)
		}
		if request.Ephemeral {
			return bidResult(chat.Claude, chat.Claude), nil
		}
		return agent.TurnResult{Text: "main work complete", Done: true}, nil
	}
	if err := orchestrator.Post("private side question?"); err != nil {
		t.Fatal(err)
	}
	waitForConversationState(t, orchestrator, chat.ConversationAnswered)
	if err := orchestrator.Post("implement the main change"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, orchestrator.Events(), nil)
}

func waitForConversationState(t *testing.T, orchestrator *Orchestrator, state chat.ConversationState) chat.ConversationJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		roomState, _ := orchestrator.Snapshot()
		for _, job := range roomState.Conversations {
			if job.State == state {
				return job
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for conversation state %s", state)
	return chat.ConversationJob{}
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

func TestOperationalStatusQuestionBypassesActiveWorkflowAndProviderCalls(t *testing.T) {
	orchestrator, codexAgent, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	orchestrator.ConfigureTemporaryAgents(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	codexAgent.run = func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.Ephemeral {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		close(started)
		select {
		case <-release:
			return agent.TurnResult{Text: "implementation complete", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}
	if err := orchestrator.Post("@codex implement the active change"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writable workflow did not start")
	}
	providerCalls := codexAgent.callCount()
	begin := time.Now()
	if err := orchestrator.Post("where are we?"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed >= time.Second {
		t.Fatalf("host status response took %s", elapsed)
	}
	if codexAgent.callCount() != providerCalls {
		t.Fatalf("status invoked provider: before=%d after=%d", providerCalls, codexAgent.callCount())
	}
	roomState, messages := orchestrator.Snapshot()
	if len(roomState.Conversations) != 1 || roomState.Conversations[0].State != chat.ConversationAnswered || !roomState.Conversations[0].Unread {
		t.Fatalf("status conversation=%+v", roomState.Conversations)
	}
	answer := messages[len(messages)-1]
	if answer.Author != chat.System || !strings.Contains(answer.Text, "workflow: active") || !strings.Contains(answer.Text, "@codex:") {
		t.Fatalf("host status answer=%+v", answer)
	}
	close(release)
}

func TestProviderEligibilityAppliesBrandHoldQuotaAndSaturation(t *testing.T) {
	orchestrator, _ := newFourAgentOrchestrator(t)
	defer orchestrator.Close()
	worker, _ := chat.AuxiliaryParticipant(chat.Agy, 1)
	workerTwo, _ := chat.AuxiliaryParticipant(chat.Agy, 2)
	orchestrator.mu.Lock()
	orchestrator.room.Members[worker] = true
	orchestrator.agents[worker] = &fakeAgent{participant: worker}
	orchestrator.agentGates[worker] = &sync.Mutex{}
	orchestrator.room.Members[workerTwo] = true
	orchestrator.agents[workerTwo] = &fakeAgent{participant: workerTwo}
	orchestrator.agentGates[workerTwo] = &sync.Mutex{}
	orchestrator.mu.Unlock()
	if err := orchestrator.SetPresence(chat.Agy, false); err != nil {
		t.Fatal(err)
	}
	if got := orchestrator.ProviderEligibility(worker); got.Eligible || !strings.Contains(got.Reason, "manual hold") {
		t.Fatalf("held auxiliary eligibility=%+v", got)
	}
	if err := orchestrator.SetPresence(chat.Agy, true); err != nil {
		t.Fatal(err)
	}
	retry := time.Now().UTC().Add(time.Hour)
	orchestrator.mu.Lock()
	orchestrator.room.Availability[chat.Agy] = chat.ParticipantAvailability{Reason: "quota", DetectedAt: time.Now().UTC(), RetryAt: &retry}
	orchestrator.mu.Unlock()
	if got := orchestrator.ProviderEligibility(worker); got.Eligible || got.Reason != "quota" {
		t.Fatalf("quota eligibility=%+v", got)
	}
	orchestrator.mu.Lock()
	delete(orchestrator.room.Availability, chat.Agy)
	_, cancel := context.WithCancel(context.Background())
	orchestrator.activeTurns[chat.Agy] = activeTurn{cancel: cancel}
	_, cancelTwo := context.WithCancel(context.Background())
	orchestrator.activeTurns[workerTwo] = activeTurn{cancel: cancelTwo}
	orchestrator.mu.Unlock()
	if got := orchestrator.ProviderEligibility(worker); got.Eligible || !strings.Contains(got.Reason, "at capacity") || !got.Waitable {
		t.Fatalf("saturated eligibility=%+v", got)
	}
	cancel()
	cancelTwo()
	orchestrator.mu.Lock()
	delete(orchestrator.activeTurns, chat.Agy)
	delete(orchestrator.activeTurns, workerTwo)
	orchestrator.mu.Unlock()
}

func TestBumpIsReadOnlyAndMarksGoneProcessNeedsAttention(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	provider := &healthAgent{fakeAgent: &fakeAgent{participant: chat.Codex}, alive: true, reason: "process alive"}
	orchestrator, err := New(roomState, nil, roomStore, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	_, cancel := context.WithCancel(context.Background())
	orchestrator.mu.Lock()
	orchestrator.activeTurns[chat.Codex] = activeTurn{cancel: cancel}
	orchestrator.setActivityLocked(chat.Codex, chat.SchedulerActive, "testing internal/room", "implement status", "lead", chat.OperationTesting, "", "", "provider_call_started", nil)
	orchestrator.mu.Unlock()
	result, err := orchestrator.Bump(chat.Codex)
	if err != nil || !result.Alive || provider.callCount() != 0 {
		t.Fatalf("live bump=%+v calls=%d err=%v", result, provider.callCount(), err)
	}
	provider.alive = false
	provider.reason = "provider process exited"
	result, err = orchestrator.Bump(chat.Codex)
	if err != nil || result.State != chat.SchedulerNeedsAttention || provider.callCount() != 0 {
		t.Fatalf("dead bump=%+v calls=%d err=%v", result, provider.callCount(), err)
	}
	cancel()
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

func TestWorkflowIdleAnnouncesOncePerRequest(t *testing.T) {
	orchestrator, _, _ := newTestOrchestrator(t)
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex please make one small change"); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(8 * time.Second)
	idle, finished := 0, 0
	for idle == 0 {
		select {
		case event := <-orchestrator.Events():
			switch event.Type {
			case EventError:
				t.Fatalf("orchestrator error: %v", event.Err)
			case EventTurnFinished:
				finished++
			case EventWorkflowIdle:
				idle++
			}
		case <-timeout:
			t.Fatalf("timed out waiting for the idle boundary: turns=%d", finished)
		}
	}
	orchestrator.wg.Wait()
	// Draining after the workflow settles must not produce a second bell: the
	// human is told once per request, however many agents took a turn.
	for {
		select {
		case event := <-orchestrator.Events():
			if event.Type == EventWorkflowIdle {
				idle++
			}
		case <-time.After(200 * time.Millisecond):
			if idle != 1 {
				t.Fatalf("idle announcements=%d want=1 (turns finished=%d)", idle, finished)
			}
			if finished == 0 {
				t.Fatal("no turn finished, so the idle boundary proves nothing")
			}
			return
		}
	}
}
