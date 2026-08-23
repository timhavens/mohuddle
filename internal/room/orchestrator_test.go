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
		input string
		text  string
	}{
		{input: "@claude?", text: "?"},
		{input: "@claude, please review", text: "please review"},
		{input: "@claude: thoughts?", text: "thoughts?"},
		{input: "@claude\tcheck this", text: "check this"},
	}
	for _, test := range tests {
		participant, text := parseTarget(test.input)
		if participant != chat.Claude || text != test.text {
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
		_, ok := providerAvailability(chat.Claude, errors.New("You've hit your session limit · resets 1:20am (ET)"), time.Now())
		if ok {
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
