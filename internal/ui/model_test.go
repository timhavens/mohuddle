package ui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/remote/device"
	"github.com/timhavens/mohuddle/internal/room"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/speech"
	"github.com/timhavens/mohuddle/internal/store"
)

type rosterTestAgent struct{ participant chat.Participant }

type uiTestResearcher struct{}

func (uiTestResearcher) Research(context.Context, chat.Participant, string, []agent.ResearchRequest) []agent.ResearchResult {
	return nil
}

type fakeRemoteDeviceStore struct {
	roomID  string
	name    string
	scopes  []device.Scope
	grants  []device.Grant
	revoked string
	updated string
}

func (s *fakeRemoteDeviceStore) CreateInvitation(roomID, name string, scopes []device.Scope, _ time.Duration) (device.Invitation, error) {
	s.roomID = roomID
	s.name = name
	s.scopes = append([]device.Scope(nil), scopes...)
	return device.Invitation{ID: "invite", Code: "PAIR-CODE", ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}

func (s *fakeRemoteDeviceStore) List() []device.Grant {
	return append([]device.Grant(nil), s.grants...)
}

func (s *fakeRemoteDeviceStore) Revoke(id string) error {
	s.revoked = id
	for index := range s.grants {
		if s.grants[index].ID == id {
			now := time.Now().UTC()
			s.grants[index].RevokedAt = &now
		}
	}
	return nil
}

func (s *fakeRemoteDeviceStore) SetScopes(id string, scopes []device.Scope) (device.Grant, error) {
	s.updated = id
	for index := range s.grants {
		if s.grants[index].ID == id {
			s.grants[index].Scopes = append([]device.Scope(nil), scopes...)
			return s.grants[index], nil
		}
	}
	return device.Grant{}, fmt.Errorf("not found")
}

func (a rosterTestAgent) Participant() chat.Participant { return a.participant }
func (a rosterTestAgent) Close() error                  { return nil }
func (a rosterTestAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: "done", Done: true}, nil
}

type blockingRosterTestAgent struct {
	participant chat.Participant
	started     chan struct{}
	release     chan struct{}
	cancelled   chan struct{}
}

func (a blockingRosterTestAgent) Participant() chat.Participant { return a.participant }
func (a blockingRosterTestAgent) Close() error                  { return nil }
func (a blockingRosterTestAgent) Run(ctx context.Context, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
	select {
	case a.started <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
		return agent.TurnResult{Text: "done", Done: true}, nil
	case <-ctx.Done():
		if a.cancelled != nil {
			select {
			case a.cancelled <- struct{}{}:
			default:
			}
		}
		return agent.TurnResult{}, ctx.Err()
	}
}

func TestEscapeCancelsActiveWork(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	orchestrator, err := room.New(roomState, nil, roomStore, blockingRosterTestAgent{
		participant: chat.Codex,
		started:     started,
		release:     release,
		cancelled:   cancelled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Post("@codex keep working"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("active work did not start")
	}
	if err := orchestrator.Post("queue this too"); err != nil {
		t.Fatal(err)
	}
	if orchestrator.PendingInputCount() != 1 {
		t.Fatalf("queued=%d", orchestrator.PendingInputCount())
	}

	model := New(orchestrator, roomStore)
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not cancel active work")
	}
	for deadline := time.Now().Add(2 * time.Second); orchestrator.HasActiveWork() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if orchestrator.HasActiveWork() {
		t.Fatal("Esc left active work running")
	}
	if orchestrator.PendingInputCount() != 0 {
		t.Fatalf("Esc left %d queued inputs", orchestrator.PendingInputCount())
	}
}

func TestShiftTabAndPlanCommandPersistWorkflowMode(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	model := New(orchestrator, roomStore)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	model = updated.(Model)
	roomCopy, _ := orchestrator.Snapshot()
	if roomCopy.WorkflowMode != chat.WorkflowPlan || !strings.Contains(model.contextFooter(), "PLAN") || !strings.Contains(model.status, "plan mode on") {
		t.Fatalf("plan toggle: mode=%q footer=%q status=%q", roomCopy.WorkflowMode, model.contextFooter(), model.status)
	}
	loaded, err := roomStore.LoadRoom(roomState.ID)
	if err != nil || loaded.WorkflowMode != chat.WorkflowPlan {
		t.Fatalf("persisted plan mode=%q err=%v", loaded.WorkflowMode, err)
	}

	model.submit("/plan status")
	if len(model.notices) == 0 || !strings.Contains(strings.ToLower(model.notices[len(model.notices)-1].Text), "plan") {
		t.Fatalf("plan status notice=%+v", model.notices)
	}
	model.submit("/plan off")
	roomCopy, _ = orchestrator.Snapshot()
	if roomCopy.WorkflowMode != chat.WorkflowExecute || strings.Contains(model.contextFooter(), "PLAN") {
		t.Fatalf("execute command: mode=%q footer=%q", roomCopy.WorkflowMode, model.contextFooter())
	}

	model.applyRoomEvent(room.Event{Type: room.EventTurnStarted, Participant: chat.Codex, WorkflowMode: chat.WorkflowPlan})
	if got := model.activity[chat.Codex].Phase; got != phasePlanning {
		t.Fatalf("plan activity phase=%q", got)
	}
}

func TestSearchCommandPersistsIndependentlyOfPlanMode(t *testing.T) {
	stateRoot := t.TempDir()
	preferences, err := appsettings.Open(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	orchestrator.ConfigureResearch(uiTestResearcher{})
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	model.submit("/search on")
	if !preferences.WebSearchEnabled() || !orchestrator.WebSearchEnabled() {
		t.Fatal("/search on did not persist")
	}
	if err := orchestrator.SetWorkflowMode(chat.WorkflowPlan); err != nil {
		t.Fatal(err)
	}
	if !orchestrator.WebSearchEnabled() {
		t.Fatal("Plan mode changed independent research setting")
	}
	model.notices = nil
	model.submit("/search status")
	if len(model.notices) != 1 || !strings.Contains(model.notices[0].Text, "is on") {
		t.Fatalf("search status=%+v", model.notices)
	}
	model.submit("/search off")
	if preferences.WebSearchEnabled() {
		t.Fatal("/search off did not persist")
	}
}

func TestPendingPlanReplacesComposerAndNoStaysInPlanMode(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	content := "# Ready plan\n\n- Make the change"
	source := chat.Message{
		ID: "source", Sequence: 1, Author: chat.Codex, Kind: chat.MessageText,
		Text: "<proposed_plan>\n" + content + "\n</proposed_plan>", CreatedAt: time.Now().UTC(),
	}
	plan := chat.ProposedPlan{
		ID: "plan", SourceMessageID: source.ID, SourceSequence: source.Sequence, Author: source.Author,
		Content: content, SHA256: chat.ProposedPlanHash(content), CreatedAt: time.Now().UTC(),
	}
	roomState.WorkflowMode = chat.WorkflowPlan
	roomState.PendingPlan = &plan
	roomState.Sessions[chat.Codex] = chat.AgentSession{ID: "planning-session", Cursor: 1}
	if err := roomStore.AppendMessage(roomState.ID, source); err != nil {
		t.Fatal(err)
	}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, []chat.Message{source}, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	model := New(orchestrator, roomStore)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	model = updated.(Model)
	view := model.View()
	if !strings.Contains(view, "Implement the plan?") || !strings.Contains(view, "Yes, implement this plan") || !strings.Contains(view, "No, stay in Plan mode") {
		t.Fatalf("pending plan composer=%q", view)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	roomCopy, _ := orchestrator.Snapshot()
	if roomCopy.PendingPlan != nil || roomCopy.WorkflowMode != chat.WorkflowPlan || roomCopy.Sessions[chat.Codex].ID != "planning-session" || !strings.Contains(model.status, "staying in Plan mode") {
		t.Fatalf("No decision room=%+v status=%q", roomCopy, model.status)
	}
}

func TestPendingPlanEnterStartsDefaultWorkflow(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	content := "# Approved plan\n\n- Execute exactly"
	source := chat.Message{
		ID: "source", Sequence: 1, Author: chat.Codex, Kind: chat.MessageText,
		Text: "<proposed_plan>\n" + content + "\n</proposed_plan>", CreatedAt: time.Now().UTC(),
	}
	plan := chat.ProposedPlan{
		ID: "plan", SourceMessageID: source.ID, SourceSequence: source.Sequence, Author: source.Author,
		Content: content, SHA256: chat.ProposedPlanHash(content), CreatedAt: time.Now().UTC(),
	}
	roomState.WorkflowMode = chat.WorkflowPlan
	roomState.PendingPlan = &plan
	if err := roomStore.AppendMessage(roomState.ID, source); err != nil {
		t.Fatal(err)
	}
	if err := roomStore.SaveRoom(roomState); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, []chat.Message{source}, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()

	model := New(orchestrator, roomStore)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	roomCopy, messages := orchestrator.Snapshot()
	if roomCopy.PendingPlan != nil || roomCopy.WorkflowMode != chat.WorkflowExecute || !strings.Contains(model.status, "accepted plan started") {
		t.Fatalf("Yes decision room=%+v status=%q", roomCopy, model.status)
	}
	accepted := false
	for _, message := range messages {
		accepted = accepted || message.AcceptedPlan != nil && message.AcceptedPlan.ID == plan.ID && message.WorkflowMode == chat.WorkflowExecute
	}
	if !accepted {
		t.Fatalf("accepted plan message missing: %+v", messages)
	}
}

func TestAmbiguousRoutingRequiresSecondConfirmationBeforeReplace(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	source := chat.Message{ID: "source", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "take another look", InputIntent: chat.InputAmbiguous, ConversationID: "route-1", CreatedAt: time.Now().UTC()}
	roomState.PendingRoutes = []uint64{source.Sequence}
	orchestrator, err := room.New(roomState, []chat.Message{source}, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.routeChoice = 2
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if !model.routeReplaceConfirm || len(snapshotPendingRoutes(orchestrator)) != 1 || !strings.Contains(strings.ToLower(model.status), "press enter again") {
		t.Fatalf("first replace press was not a confirmation boundary: confirm=%v routes=%v view=%q", model.routeReplaceConfirm, snapshotPendingRoutes(orchestrator), model.routeDecisionView())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if model.routeReplaceConfirm || len(snapshotPendingRoutes(orchestrator)) != 0 {
		t.Fatalf("confirmed replacement did not route work: confirm=%v routes=%v", model.routeReplaceConfirm, snapshotPendingRoutes(orchestrator))
	}
}

func TestNeedsAttentionConversationIsPinnedAndCanKeepWaiting(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source := chat.Message{ID: "source", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "what are the stats?", InputIntent: chat.InputConversation, ConversationID: "conversation-1", CreatedAt: now}
	roomState.Conversations = []chat.ConversationJob{{ID: "conversation-1", SourceSequence: 1, State: chat.ConversationNeedsAttention, Class: chat.ConversationQuick, TerminalReason: "response deadline expired", CreatedAt: now, UpdatedAt: now, LastActivityAt: now}}
	orchestrator, err := room.New(roomState, []chat.Message{source}, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 32})
	model = updated.(Model)
	if view := model.View(); !strings.Contains(view, "CHAT NEEDS ATTENTION") || !strings.Contains(view, "Alt+K keep waiting") || !strings.Contains(view, "response deadline expired") {
		t.Fatalf("needs-attention pin=%q", view)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}, Alt: true})
	model = updated.(Model)
	job := snapshotConversation(orchestrator, "conversation-1")
	if job.State != chat.ConversationFinding || job.Deadline == nil || job.TerminalReason != "" {
		t.Fatalf("keep-waiting state=%+v", job)
	}
}

func TestRepliesCommandPersistsTemporaryResponderLimit(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{participant: chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	orchestrator.ConfigureTemporaryAgents(nil)
	model := New(orchestrator, roomStore)
	model.submit("/replies 0")
	if got := preferences.ConversationResponderLimit(); got != 0 || orchestrator.ConversationResponderLimit() != 0 {
		t.Fatalf("responder limit preference=%d runtime=%d", got, orchestrator.ConversationResponderLimit())
	}
	model.submit("/replies 9")
	if got := preferences.ConversationResponderLimit(); got != 0 {
		t.Fatalf("invalid reply limit mutated preference=%d", got)
	}
}

func snapshotPendingRoutes(orchestrator *room.Orchestrator) []uint64 {
	value, _ := orchestrator.Snapshot()
	return value.PendingRoutes
}

func snapshotConversation(orchestrator *room.Orchestrator, id string) chat.ConversationJob {
	value, _ := orchestrator.Snapshot()
	for _, job := range value.Conversations {
		if job.ID == id {
			return job
		}
	}
	return chat.ConversationJob{}
}

type workerTestPreferences struct {
	counts   map[chat.Participant]int
	setCalls int
	progress chat.ProgressMode
}

func (p *workerTestPreferences) Default(participant chat.Participant) chat.AgentSettings {
	return appsettings.BuiltIn(participant)
}

func (p *workerTestPreferences) Effective(roomState chat.Room, participant chat.Participant) chat.AgentSettings {
	if value, ok := roomState.Settings[participant]; ok {
		return appsettings.NormalizeFor(participant, value)
	}
	return p.Default(participant)
}

func (p *workerTestPreferences) SetDefault(chat.Participant, chat.AgentSettings) error { return nil }
func (p *workerTestPreferences) FullAccessAcknowledged() bool                          { return false }
func (p *workerTestPreferences) AcknowledgeFullAccess() error                          { return nil }
func (p *workerTestPreferences) DetailsVisible() bool                                  { return false }
func (p *workerTestPreferences) SetDetailsVisible(bool) error                          { return nil }
func (p *workerTestPreferences) ProgressDisplayMode() chat.ProgressMode {
	return p.progress.WithDefault()
}
func (p *workerTestPreferences) SetProgressDisplayMode(mode chat.ProgressMode) error {
	p.progress = mode
	return nil
}

func (p *workerTestPreferences) WorkerCounts() map[chat.Participant]int {
	result := make(map[chat.Participant]int, len(p.counts))
	for participant, count := range p.counts {
		result[participant] = count
	}
	return result
}

func (p *workerTestPreferences) SetWorkerCounts(counts map[chat.Participant]int) error {
	p.setCalls++
	p.counts = make(map[chat.Participant]int, len(counts))
	for participant, count := range counts {
		p.counts[participant] = count
	}
	return nil
}

type spokenMessage struct {
	agent chat.Participant
	text  string
}

type fakeCompletionNotifier struct {
	calls int
	err   error
}

func noticesText(values []noticeEntry) string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, value.Text)
	}
	return strings.Join(lines, "\n")
}

func (f *fakeCompletionNotifier) Notify() error {
	f.calls++
	return f.err
}

type fakeSpeechController struct {
	state  speech.State
	spoken []spokenMessage
	events chan speech.Event
}

func newFakeSpeechController() *fakeSpeechController {
	config := speech.Config{Mode: speech.ModeAll, MaxChunkChars: speech.DefaultChunkChars}.WithDefaults()
	return &fakeSpeechController{state: speech.State{Config: config, Available: true}, events: make(chan speech.Event, 8)}
}

func (f *fakeSpeechController) Speak(agent chat.Participant, text string) {
	f.spoken = append(f.spoken, spokenMessage{agent: agent, text: text})
}
func (f *fakeSpeechController) SetEnabled(enabled bool) error {
	f.state.Config.Enabled = enabled
	return nil
}
func (f *fakeSpeechController) SetSelection(mode speech.Mode, agent chat.Participant) error {
	f.state.Config.Enabled = true
	f.state.Config.Mode = mode
	f.state.Config.Agent = agent
	return nil
}
func (f *fakeSpeechController) SetVoice(agent chat.Participant, voice string) error {
	if f.state.Config.Voices == nil {
		f.state.Config.Voices = map[chat.Participant]string{}
	}
	if voice == "" {
		delete(f.state.Config.Voices, agent)
	} else {
		f.state.Config.Voices[agent] = voice
	}
	return nil
}
func (f *fakeSpeechController) Stop()                       { f.state.Queued = 0; f.state.Speaking = false }
func (f *fakeSpeechController) Skip()                       { f.state.Speaking = false }
func (f *fakeSpeechController) Snapshot() speech.State      { return f.state }
func (f *fakeSpeechController) Events() <-chan speech.Event { return f.events }
func (f *fakeSpeechController) ListVoices(context.Context, string) ([]speech.Voice, error) {
	return []speech.Voice{{Name: "en-US-TestNeural", Description: "test"}}, nil
}
func (f *fakeSpeechController) Close() error { return nil }

func TestPublicLiveTextHidesControlMarker(t *testing.T) {
	value := "public response\n<!-- mohuddle:{\"done\":true} -->"
	if got := publicLiveText(value); got != "public response\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCorrectionStatusLinesShowRoomAndPerAgentCounts(t *testing.T) {
	now := time.Now().UTC()
	lines := correctionStatusLines([]chat.Message{
		{Sequence: 1, Author: chat.Claude, Kind: chat.MessageText, Text: "claim", CreatedAt: now},
		{Sequence: 2, Author: chat.Codex, Kind: chat.MessageText, Text: "correction", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionOffered, CorrectionSequence: 2, CorrectedSequence: 1, Proposer: chat.Codex, Target: chat.Claude}}},
		{Sequence: 3, Author: chat.Claude, Kind: chat.MessageText, Text: "accepted", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionAccepted, CorrectionSequence: 2}}},
		{Sequence: 4, Author: chat.Codex, Kind: chat.MessageText, Text: "claim", CreatedAt: now},
		{Sequence: 5, Author: chat.Claude, Kind: chat.MessageText, Text: "correction", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionOffered, CorrectionSequence: 5, CorrectedSequence: 4, Proposer: chat.Claude, Target: chat.Codex}}},
		{Sequence: 6, Author: chat.Claude, Kind: chat.MessageText, Text: "retracted", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionRetracted, CorrectionSequence: 5}}},
	})
	joined := strings.Join(lines, "\n")
	for _, expected := range []string{
		"corrections: offered 2; accepted 1; retracted 1; pending 0",
		"corrections @codex: offered 1; accepted 1; retracted 0; pending 0; accepted received 0",
		"corrections @claude: offered 1; accepted 0; retracted 1; pending 0; accepted received 1",
		"corrections @agy: offered 0; accepted 0; retracted 0; pending 0; accepted received 0",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("status missing %q:\n%s", expected, joined)
		}
	}
}

func TestStatusCommandIncludesCorrectionStatistics(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	messages := []chat.Message{
		{Sequence: 1, Author: chat.Claude, Kind: chat.MessageText, Text: "claim", CreatedAt: now},
		{Sequence: 2, Author: chat.Codex, Kind: chat.MessageText, Text: "correction", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionOffered, CorrectionSequence: 2, CorrectedSequence: 1, Proposer: chat.Codex, Target: chat.Claude}}},
		{Sequence: 3, Author: chat.Claude, Kind: chat.MessageText, Text: "accepted", CreatedAt: now, CorrectionEvents: []chat.CorrectionEvent{{Type: chat.CorrectionAccepted, CorrectionSequence: 2}}},
	}
	orchestrator, err := room.New(roomState, messages, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/status")
	joined := make([]string, 0, len(model.notices))
	for _, notice := range model.notices {
		joined = append(joined, notice.Text)
	}
	status := strings.Join(joined, "\n")
	if !strings.Contains(status, "corrections: offered 1; accepted 1; retracted 0; pending 0") || !strings.Contains(status, "corrections @claude: offered 0; accepted 0; retracted 0; pending 0; accepted received 1") {
		t.Fatalf("status output=%q", status)
	}
}

func TestComposerHistoryPreservesAndRestoresDraft(t *testing.T) {
	input := textarea.New()
	input.SetValue("unfinished draft")
	model := Model{
		input: input,
		history: []chat.ComposerHistoryEntry{
			{Text: "first prompt"},
			{Text: "second prompt", Pastes: []string{"pasted block"}},
		},
		historyIndex: 2,
		pastes:       []string{"draft paste"},
	}
	if !model.browseHistory(-1) || model.input.Value() != "second prompt" || len(model.pastes) != 1 || model.pastes[0] != "pasted block" {
		t.Fatalf("recalled composer=%q pastes=%v", model.input.Value(), model.pastes)
	}
	model.browseHistory(1)
	if model.input.Value() != "unfinished draft" || len(model.pastes) != 1 || model.pastes[0] != "draft paste" {
		t.Fatalf("restored draft=%q pastes=%v", model.input.Value(), model.pastes)
	}
}

func TestMultilinePasteBecomesCompactComposerItem(t *testing.T) {
	input := textarea.New()
	input.SetValue("please review")
	model := Model{input: input}
	model.addPastedText("line one\nline two")
	if len(model.pastes) != 1 || !strings.Contains(model.composerItemsView(), "Pasted Content 1") {
		t.Fatalf("pastes=%v view=%q", model.pastes, model.composerItemsView())
	}
	if got := model.composedText(); got != "please review\n\nline one\nline two" {
		t.Fatalf("composed text=%q", got)
	}
}

func TestTranscriptScrollDoesNotLoseComposerOrAutoFollow(t *testing.T) {
	input := textarea.New()
	input.SetValue("keep my draft")
	input.Focus()
	model := Model{
		input: input, viewport: viewport.New(50, 4), ready: true, following: true, width: 50,
		activity: map[chat.Participant]participantActivity{}, live: map[chat.Participant]string{},
	}
	for index := 0; index < 12; index++ {
		model.messages = append(model.messages, chat.Message{Sequence: uint64(index + 1), Author: chat.Codex, Kind: chat.MessageText, Text: "a transcript line", CreatedAt: time.Now().Add(time.Duration(index) * time.Second)})
	}
	model.refreshContent()
	if !model.viewport.AtBottom() {
		t.Fatal("initial transcript did not follow the bottom")
	}
	model.handleTranscriptKey("pgup")
	if model.following {
		t.Fatal("page up did not suspend auto-follow")
	}
	message := chat.Message{Sequence: 20, Author: chat.Claude, Kind: chat.MessageText, Text: "new response", CreatedAt: time.Now().Add(time.Minute)}
	model.applyRoomEvent(room.Event{Type: room.EventMessage, Message: &message})
	if model.input.Value() != "keep my draft" || model.unseen != 1 || model.viewport.AtBottom() {
		t.Fatalf("draft=%q unseen=%d bottom=%v", model.input.Value(), model.unseen, model.viewport.AtBottom())
	}
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!")})
	if !strings.Contains(model.input.Value(), "!") {
		t.Fatalf("composer lost focus/value: %q", model.input.Value())
	}
	model.handleTranscriptKey("ctrl+end")
	if !model.following || model.unseen != 0 {
		t.Fatalf("following=%v unseen=%d", model.following, model.unseen)
	}
}

func TestClipboardImageIsSavedAsRoomAttachment(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, color.RGBA{R: 20, G: 30, B: 40, A: 255})
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	model := Model{room: roomState, composerStore: roomStore, input: textarea.New()}
	if err := model.acceptClipboardImage(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	if len(model.attachments) != 1 || model.attachments[0].Width != 1 || model.attachments[0].Height != 1 {
		t.Fatalf("attachments=%+v", model.attachments)
	}
}

func TestComposerUsesCompactUnnumberedInput(t *testing.T) {
	input := newComposerInput()
	if input.ShowLineNumbers || input.Prompt != "› " || input.Height() != 1 {
		t.Fatalf("lineNumbers=%v prompt=%q height=%d", input.ShowLineNumbers, input.Prompt, input.Height())
	}
	input.SetWidth(78)
	styled := reapplyTerminalStyle("placeholder<reset>   ", "<background>", "<reset>")
	wanted := "<background>placeholder<reset><background>   <reset>"
	if styled != wanted {
		t.Fatalf("reapplied style=%q want %q", styled, wanted)
	}
	view := Model{input: input, width: 80}.composerView()
	lines := strings.Split(view, "\n")
	if len(lines) != 3 {
		t.Fatalf("composer lines=%d view=%q", len(lines), view)
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width != 80 {
			t.Fatalf("composer width=%d want 80; view=%q", width, view)
		}
	}
}

func TestViewWaitsForInitialWindowSize(t *testing.T) {
	model := Model{
		input:    newComposerInput(),
		viewport: viewport.New(80, 20),
		width:    80,
		height:   24,
		activity: map[chat.Participant]participantActivity{},
		live:     map[chat.Participant]string{},
	}
	if view := model.View(); view != "" {
		t.Fatalf("view rendered before initial resize: %q", view)
	}
	model.resize()
	if view := model.View(); view == "" {
		t.Fatal("view remained empty after initial resize")
	}
}

func TestAltMTogglesMouseCapture(t *testing.T) {
	model := Model{mouseCaptured: true, width: 120}
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}, Alt: true}

	updated, command := model.Update(key)
	model = updated.(Model)
	if model.mouseCaptured || model.status != "text selection enabled" || command == nil {
		t.Fatalf("disabled state: captured=%v status=%q command=%v", model.mouseCaptured, model.status, command)
	}
	if footer := model.keyFooter(); !strings.Contains(footer, "mouse=select") {
		t.Fatalf("selection mode missing from footer: %q", footer)
	}

	updated, command = model.Update(key)
	model = updated.(Model)
	if !model.mouseCaptured || model.status != "mouse scroll enabled" || command == nil {
		t.Fatalf("enabled state: captured=%v status=%q command=%v", model.mouseCaptured, model.status, command)
	}
	if footer := model.keyFooter(); !strings.Contains(footer, "mouse=scroll") {
		t.Fatalf("scroll mode missing from footer: %q", footer)
	}
}

func TestContextFooterShowsBothCoreAgents(t *testing.T) {
	input := textarea.New()
	input.SetValue("@claude review this")
	model := Model{input: input, room: chat.Room{Workspace: "/work/project", Moderator: chat.Codex}, width: 100}
	footer := model.contextFooter()
	for _, wanted := range []string{"CODEX", "CLAUDE", "default", "auto", "workspace", "/work/project"} {
		if !strings.Contains(footer, wanted) {
			t.Fatalf("footer missing %q: %q", wanted, footer)
		}
	}
}

func TestActivityTracksSilentWorkToolsAndCompletion(t *testing.T) {
	started := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	model := Model{
		activity:     map[chat.Participant]participantActivity{},
		live:         map[chat.Participant]string{},
		now:          started,
		width:        100,
		showDetails:  true,
		progressMode: chat.ProgressDetailed,
	}

	model.applyRoomEvent(room.Event{Type: room.EventTurnStarted, Participant: chat.Codex})
	if got := model.activity[chat.Codex].Phase; got != phaseThinking {
		t.Fatalf("start phase=%q", got)
	}

	model.now = started.Add(5 * time.Second)
	model.applyRoomEvent(room.Event{Type: room.EventAgent, AgentEvent: &agent.Event{
		Type: agent.EventTool, Agent: chat.Codex, Text: "running go test ./...",
	}})
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "testing") || !strings.Contains(line, "5s") || !strings.Contains(line, "go test ./...") {
		t.Fatalf("activity line=%q", line)
	}

	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Codex})
	activity := model.activity[chat.Codex]
	if activity.Phase != phaseIdle || activity.Detail != "running go test ./..." || !activity.StartedAt.IsZero() {
		t.Fatalf("finished activity=%+v", activity)
	}
}

func TestDelegationDoneFinishesOnlyAssignedWorker(t *testing.T) {
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	model := Model{
		activity: map[chat.Participant]participantActivity{
			chat.Codex: {Phase: phaseThinking, Detail: "main task"},
			worker:     {Phase: phaseThinking, Detail: "delegated task"},
		},
		live: map[chat.Participant]string{},
	}
	model.applyRoomEvent(room.Event{Type: room.EventDelegationDone, Participant: worker, Text: "worker complete"})
	if model.activity[worker].Phase != phaseIdle {
		t.Fatalf("worker activity=%+v", model.activity[worker])
	}
	if model.activity[chat.Codex].Phase != phaseThinking {
		t.Fatalf("main activity was cleared by helper completion: %+v", model.activity[chat.Codex])
	}
	if model.status != "worker complete" {
		t.Fatalf("status=%q", model.status)
	}
}

func TestTurnFinishedRingsOnlyWhenCompletionSoundEnabled(t *testing.T) {
	notifier := &fakeCompletionNotifier{}
	model := Model{
		activity:           map[chat.Participant]participantActivity{},
		live:               map[chat.Participant]string{},
		now:                time.Now(),
		completionSound:    true,
		completionNotifier: notifier,
	}
	model.applyRoomEvent(room.Event{Type: room.EventTurnStarted, Participant: chat.Codex})
	if notifier.calls != 0 {
		t.Fatalf("sound rang before completion: calls=%d", notifier.calls)
	}
	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Codex})
	if notifier.calls != 1 {
		t.Fatalf("sound calls=%d want=1", notifier.calls)
	}
	model.completionSound = false
	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Claude})
	if notifier.calls != 1 {
		t.Fatalf("disabled sound calls=%d want=1", notifier.calls)
	}
}

func TestCompletionSoundFailureIsReportedOnce(t *testing.T) {
	notifier := &fakeCompletionNotifier{err: fmt.Errorf("bell unavailable")}
	model := Model{
		activity:           map[chat.Participant]participantActivity{},
		live:               map[chat.Participant]string{},
		now:                time.Now(),
		completionSound:    true,
		completionNotifier: notifier,
	}
	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Codex})
	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Claude})
	if notifier.calls != 2 {
		t.Fatalf("notifier calls=%d want=2", notifier.calls)
	}
	if len(model.notices) != 1 || !strings.Contains(model.notices[0].Text, "bell unavailable") {
		t.Fatalf("notices=%v", model.notices)
	}
}

func TestSoundCommandPersistsAndAppearsInSettings(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	if model.completionSound {
		t.Fatal("completion sound should start disabled")
	}
	model.submit("/sound on")
	if !model.completionSound || !preferences.CompletionSoundEnabled() {
		t.Fatalf("sound state model=%t persisted=%t", model.completionSound, preferences.CompletionSoundEnabled())
	}
	model.notices = nil
	model.showSettings()
	if len(model.notices) != 1 || !strings.Contains(model.notices[0].Text, "AI-finished terminal sound: on") {
		t.Fatalf("settings notice=%v", model.notices)
	}
}

func TestWorkersCommandPersistsAtomicallyAndReloadsCurrentRoom(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	command := model.submit("/workers @codex 2 @claude 1")
	if command == nil || !model.quitting || model.action.ResumeID != roomState.ID {
		t.Fatalf("command=%v quitting=%t action=%+v", command, model.quitting, model.action)
	}
	counts := preferences.WorkerCounts()
	if counts[chat.Codex] != 2 || counts[chat.Claude] != 1 || counts[chat.Agy] != 0 || counts[chat.Copilot] != 0 {
		t.Fatalf("persisted counts=%v", counts)
	}
}

func TestWorkersCommandInvalidBatchDoesNotPartiallyPersist(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCounts(map[chat.Participant]int{chat.Codex: 1}); err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	if command := model.submit("/workers @codex 2 @claude invalid"); command != nil || model.quitting {
		t.Fatalf("invalid update command=%v quitting=%t", command, model.quitting)
	}
	counts := preferences.WorkerCounts()
	if counts[chat.Codex] != 1 || counts[chat.Claude] != 0 || counts[chat.Agy] != 0 || counts[chat.Copilot] != 0 {
		t.Fatalf("invalid batch changed persisted counts=%v", counts)
	}
}

func TestWorkersCommandSameValueDoesNotSaveOrReload(t *testing.T) {
	preferences := &workerTestPreferences{counts: map[chat.Participant]int{chat.Codex: 1}}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	orchestrator, err := room.New(roomState, nil, roomStore, blockingRosterTestAgent{participant: chat.Codex, started: started, release: release})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Post("@codex keep working"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("active work did not start")
	}

	model := New(orchestrator, roomStore)
	if command := model.submit("/workers @codex 1"); command != nil {
		t.Fatalf("unchanged workers returned reload command %v", command)
	}
	if model.quitting || model.action != (ExitAction{}) || preferences.setCalls != 0 {
		t.Fatalf("quitting=%t action=%+v settings saves=%d", model.quitting, model.action, preferences.setCalls)
	}
	if len(model.notices) == 0 || !strings.Contains(model.notices[len(model.notices)-1].Text, "unchanged") {
		t.Fatalf("notices=%v", model.notices)
	}
	close(release)
}

func TestWorkersShowDisplaysTopologyWithoutSavingOrReloading(t *testing.T) {
	preferences := &workerTestPreferences{counts: map[chat.Participant]int{chat.Codex: 1, chat.Claude: 1}}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}

	model := New(orchestrator, roomStore)
	if command := model.submit("/workers show"); command != nil {
		t.Fatalf("workers show returned reload command %v", command)
	}
	if model.quitting || model.action != (ExitAction{}) || preferences.setCalls != 0 {
		t.Fatalf("quitting=%t action=%+v settings saves=%d", model.quitting, model.action, preferences.setCalls)
	}
	joined := make([]string, 0, len(model.notices))
	for _, notice := range model.notices {
		joined = append(joined, notice.Text)
	}
	output := strings.Join(joined, "\n")
	for _, expected := range []string{"Auxiliary workers: 2 total", "@codex-1", "@claude-1"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("workers show missing %q:\n%s", expected, output)
		}
	}
}

func TestRosterCommandsScheduleShowAndCancelAuthorizedWorkerAction(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = false
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/roster schedule join @codex-1 for 1h quota retry")
	actions := orchestrator.RosterActions()
	if len(actions) != 1 || actions[0].Action != chat.RosterActionJoin || actions[0].Participant != worker || actions[0].AuthorizedBy != chat.User || actions[0].Reason != "quota retry" {
		t.Fatalf("scheduled actions=%+v", actions)
	}
	output := noticesText(model.notices)
	for _, expected := range []string{"Scheduled roster actions", actions[0].ID, "quota retry", "authorized by user"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("scheduled roster output missing %q:\n%s", expected, output)
		}
	}
	model.notices = nil
	model.submit("/status")
	if output := noticesText(model.notices); !strings.Contains(output, "scheduled roster:") || !strings.Contains(output, actions[0].ID) {
		t.Fatalf("status omitted scheduled roster action:\n%s", output)
	}
	model.notices = nil
	model.submit("/roster cancel " + actions[0].ID)
	actions = orchestrator.RosterActions()
	if actions[0].Status != chat.RosterActionCancelled || actions[0].CompletedAt == nil {
		t.Fatalf("cancelled action=%+v", actions[0])
	}
	if output := noticesText(model.notices); !strings.Contains(output, "cancelled") {
		t.Fatalf("cancel output=%s", output)
	}
}

func TestRosterRetryScheduleRequiresConfirmedFutureRetry(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Codex, 1)
	roomState.Members[worker] = false
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/roster schedule join @codex-1 retry")
	if output := noticesText(model.notices); !strings.Contains(output, "no future confirmed retry time") {
		t.Fatalf("missing retry validation: %s", output)
	}
	retryAt := time.Now().Add(time.Hour).UTC()
	if err := orchestrator.SetParticipantAvailability(worker, &chat.ParticipantAvailability{Reason: "quota", Source: "test", DetectedAt: time.Now().UTC(), RetryAt: &retryAt, Confidence: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	model.notices = nil
	model.submit("/roster schedule join @codex-1 retry")
	actions := orchestrator.RosterActions()
	if len(actions) != 1 || !actions[0].ExecuteAt.Equal(retryAt) {
		t.Fatalf("retry-scheduled action=%+v", actions)
	}
}

func TestRemoteCommandsCreateLeastPrivilegeInvitationListAndRevoke(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	remoteStore := &fakeRemoteDeviceStore{grants: []device.Grant{{
		ID: "device-123", Name: "existing phone", RoomID: roomState.ID,
		Scopes: []device.Scope{device.ScopeObserve}, PermissionCeiling: device.CeilingReadOnly,
		CreatedAt: time.Now().UTC(),
	}}}
	model := New(orchestrator, roomStore)
	model.ConfigureRemote(remoteStore, "https://phone.example", nil)

	model.submit("/remote pair admin Tim's phone")
	if remoteStore.roomID != roomState.ID || remoteStore.name != "Tim's phone" || len(remoteStore.scopes) != 3 || remoteStore.scopes[2] != device.ScopeAdmin {
		t.Fatalf("pair room=%q name=%q scopes=%v", remoteStore.roomID, remoteStore.name, remoteStore.scopes)
	}
	output := noticesText(model.notices)
	for _, expected := range []string{"PAIR-CODE", "https://phone.example/#code=PAIR-CODE", "read-only"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("pair output missing %q:\n%s", expected, output)
		}
	}

	model.notices = nil
	model.submit("/remote devices")
	output = noticesText(model.notices)
	for _, expected := range []string{"existing phone", "device-123", "observe", "read-only"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("device output missing %q:\n%s", expected, output)
		}
	}

	model.notices = nil
	model.submit("/remote scope device-123 admin")
	if remoteStore.updated != "device-123" || len(remoteStore.grants[0].Scopes) != 3 || !strings.Contains(noticesText(model.notices), "prior sessions were closed") {
		t.Fatalf("scope update=%q scopes=%v output=%s", remoteStore.updated, remoteStore.grants[0].Scopes, noticesText(model.notices))
	}

	model.notices = nil
	model.submit("/remote revoke device-123")
	if remoteStore.revoked != "device-123" || !strings.Contains(noticesText(model.notices), "active sessions were closed") {
		t.Fatalf("revoke=%q output=%s", remoteStore.revoked, noticesText(model.notices))
	}

	model.notices = nil
	model.submit("/status")
	if output := noticesText(model.notices); !strings.Contains(output, "devices active 0, revoked 1") || !strings.Contains(output, "ceiling read-only") {
		t.Fatalf("status=%s", output)
	}
}

func TestConfiguredUnavailableWorkerAppearsInAgentsStatusAndSettings(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCounts(map[chat.Participant]int{chat.Claude: 1}); err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	roomState.Members[worker] = true
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}

	model := New(orchestrator, roomStore)
	checks := []struct {
		command  string
		expected []string
	}{
		{"/agents", []string{"CLAUDE-1", "auxiliary worker", "unavailable (CLI not found)"}},
		{"/status", []string{"CLAUDE-1: unavailable (provider CLI/runtime)", "read-only"}},
		{"/settings", []string{"CLAUDE-1", "read-only"}},
	}
	for _, check := range checks {
		model.notices = nil
		model.submit(check.command)
		lines := make([]string, 0, len(model.notices))
		for _, notice := range model.notices {
			lines = append(lines, notice.Text)
		}
		output := strings.Join(lines, "\n")
		for _, expected := range check.expected {
			if !strings.Contains(output, expected) {
				t.Fatalf("%s missing %q:\n%s", check.command, expected, output)
			}
		}
	}
}

func TestAllModelAndEffortChangesKeepUnavailableAuxiliaryReadOnly(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCounts(map[chat.Participant]int{chat.Claude: 1}); err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	worker, _ := chat.AuxiliaryParticipant(chat.Claude, 1)
	model := New(orchestrator, roomStore)

	model.submit("/model @all room-model")
	model.submit("/effort @all high")
	roomSettings := orchestrator.RoomSettings()[worker]
	if roomSettings.Model != "room-model" || roomSettings.Effort != "high" || roomSettings.Permissions != chat.PermissionReadOnly {
		t.Fatalf("room auxiliary settings=%+v", roomSettings)
	}

	model.submit("/model default @all personal-model")
	model.submit("/effort default @all medium")
	defaults := preferences.Default(worker)
	if defaults.Model != "personal-model" || defaults.Effort != "medium" || defaults.Permissions != chat.PermissionReadOnly {
		t.Fatalf("default auxiliary settings=%+v", defaults)
	}
}

func TestDelegateCommandRunsAuxiliaryWithoutSwitchingRooms(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker := chat.Participant("codex-1")
	roomState.Members[worker] = true
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	if command := model.submit("/delegate @codex-1 inspect the parser"); command != nil {
		t.Fatalf("delegate unexpectedly returned TUI command %v", command)
	}
	if model.quitting || model.action != (ExitAction{}) || model.status != "subtask delegated to codex-1" {
		t.Fatalf("quitting=%t action=%+v status=%q", model.quitting, model.action, model.status)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, messages := orchestrator.Snapshot()
		found := false
		for _, message := range messages {
			if message.Author == worker && message.Kind == chat.MessageText && message.Text == "done" {
				found = true
			}
			if message.Author == chat.User {
				t.Fatalf("delegate command was posted as a user message: %+v", message)
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for delegated response; messages=%v", messages)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAgentsAndSettingsShowConfiguredAuxiliaryWorker(t *testing.T) {
	preferences, err := appsettings.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWorkerCounts(map[chat.Participant]int{chat.Codex: 1}); err != nil {
		t.Fatal(err)
	}
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	worker := chat.Participant("codex-1")
	roomState.Members[worker] = true
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex}, rosterTestAgent{worker})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	model.showAgents()
	model.showSettings()
	var lines []string
	for _, notice := range model.notices {
		lines = append(lines, notice.Text)
	}
	joined := strings.Join(lines, "\n")
	for _, expected := range []string{"CODEX-1", "auxiliary worker", "Auxiliary workers: 1 total", "read-only"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("output missing %q:\n%s", expected, joined)
		}
	}
}

func TestUserMessageQueuesOnlyTargetedAgent(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{},
		live:     map[chat.Participant]string{},
		now:      time.Now(),
	}
	message := chat.Message{Author: chat.User, Target: chat.Claude, Kind: chat.MessageText, Text: "please answer"}
	model.applyRoomEvent(room.Event{Type: room.EventMessage, Message: &message})

	if got := model.activity[chat.Claude].Phase; got != phaseQueued {
		t.Fatalf("Claude phase=%q", got)
	}
	if got := model.activity[chat.Codex].Phase; got != "" && got != phaseIdle {
		t.Fatalf("Codex phase=%q", got)
	}
}

func TestActivityQueuesOnlyHostScheduledParticipants(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{},
		live:     map[chat.Participant]string{},
		now:      time.Now(),
	}
	message := chat.Message{Author: chat.User, Kind: chat.MessageText, Text: "ask the room"}
	model.applyRoomEvent(room.Event{Type: room.EventMessage, Message: &message})
	for _, participant := range chat.Agents() {
		if model.activity[participant].Phase == phaseQueued {
			t.Fatalf("%s was speculatively queued", participant)
		}
	}
	model.applyRoomEvent(room.Event{Type: room.EventRoutingStarted, Text: "choosing the core lead"})
	if model.status != "choosing the core lead" {
		t.Fatalf("routing status=%q", model.status)
	}
	model.applyRoomEvent(room.Event{Type: room.EventWaveStarted, Participants: []chat.Participant{chat.Claude, chat.Codex}})
	if model.activity[chat.Claude].Phase != phaseQueued || model.activity[chat.Codex].Phase != phaseQueued {
		t.Fatalf("scheduled core activity=%+v", model.activity)
	}
	for _, participant := range []chat.Participant{chat.Agy, chat.Copilot} {
		if model.activity[participant].Phase == phaseQueued {
			t.Fatalf("unscheduled %s was queued", participant)
		}
	}
}

func TestCompletedAgentMessagesAreHandedToSpeech(t *testing.T) {
	controller := newFakeSpeechController()
	model := Model{
		speech: controller, speechState: controller.Snapshot(),
		activity: map[chat.Participant]participantActivity{}, live: map[chat.Participant]string{},
	}
	for _, message := range []chat.Message{
		{Author: chat.Codex, Kind: chat.MessageText, Text: "speak this"},
		{Author: chat.Codex, Kind: chat.MessageTool, Text: "do not speak tool output"},
		{Author: chat.Claude, Kind: chat.MessageInterrupted, Text: "do not speak draft"},
		{Author: chat.User, Kind: chat.MessageText, Text: "do not speak user"},
	} {
		copy := message
		model.applyRoomEvent(room.Event{Type: room.EventMessage, Message: &copy})
	}
	if len(controller.spoken) != 1 || controller.spoken[0].agent != chat.Codex || controller.spoken[0].text != "speak this" {
		t.Fatalf("spoken=%+v", controller.spoken)
	}
}

func TestSpeechCommandsToggleSelectAndAssignVoice(t *testing.T) {
	controller := newFakeSpeechController()
	model := Model{speech: controller, speechState: controller.Snapshot(), activity: map[chat.Participant]participantActivity{}, live: map[chat.Participant]string{}}
	model.submit("/voice @codex en-US-AndrewMultilingualNeural")
	if got := controller.state.Config.Voices[chat.Codex]; got != "en-US-AndrewMultilingualNeural" {
		t.Fatalf("voice=%q", got)
	}
	model.submit("/speak @codex")
	if !controller.state.Config.Enabled || controller.state.Config.Mode != speech.ModeAgent || controller.state.Config.Agent != chat.Codex {
		t.Fatalf("selection=%+v", controller.state.Config)
	}
	if badge := model.speechBadge(); !strings.Contains(badge, "CODEX") {
		t.Fatalf("badge=%q", badge)
	}
	model.toggleSpeech()
	if controller.state.Config.Enabled {
		t.Fatal("speech did not toggle off")
	}
}

func TestStopClearsBusyIndicators(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{},
		now:      time.Now(),
	}
	model.setActivity(chat.Codex, phaseThinking, "waiting")
	model.queueActivity(chat.Claude)
	model.stopActivities()

	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		activity := model.activity[participant]
		if activity.Phase != phaseIdle || activity.Detail != "stopped" || !activity.StartedAt.IsZero() {
			t.Fatalf("%s activity=%+v", participant, activity)
		}
	}
}

func TestTurnFinishedClearsMarkerOnlyLiveBuffer(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{},
		live: map[chat.Participant]string{
			chat.Codex: `<!-- mohuddle:{"done":true} -->`,
		},
		now: time.Now(),
	}
	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Codex})
	if _, exists := model.live[chat.Codex]; exists {
		t.Fatal("finished marker-only stream remained in live buffer")
	}
}

func TestFormatElapsed(t *testing.T) {
	for _, test := range []struct {
		elapsed time.Duration
		want    string
	}{
		{elapsed: 9 * time.Second, want: "9s"},
		{elapsed: 83 * time.Second, want: "1m23s"},
		{elapsed: 2*time.Hour + 4*time.Minute, want: "2h04m"},
	} {
		if got := formatElapsed(test.elapsed); got != test.want {
			t.Fatalf("formatElapsed(%s)=%q want %q", test.elapsed, got, test.want)
		}
	}
}

func TestParseSettingsChangeSupportsDefaultsAndAllAgents(t *testing.T) {
	change, err := parseSettingsChange("/permissions", []string{"/permissions", "default", "@all", "full"})
	if err != nil {
		t.Fatal(err)
	}
	if !change.Default || change.Field != "permissions" || change.Value != "full" || len(change.Participants) != 4 {
		t.Fatalf("change=%+v", change)
	}
	if _, err := parseSettingsChange("/permissions", []string{"/permissions", "@codex", "unknown"}); err == nil {
		t.Fatal("invalid permission profile was accepted")
	}
}

func TestParseWorkerCountsIsAtomicAndSupportsProviderPairs(t *testing.T) {
	current := map[chat.Participant]int{chat.Codex: 1, chat.Agy: 1}
	got, err := parseWorkerCounts([]string{"/workers", "@codex", "2", "@claude", "1", "@agy", "0"}, current)
	if err != nil {
		t.Fatal(err)
	}
	if got[chat.Codex] != 2 || got[chat.Claude] != 1 || got[chat.Agy] != 0 || len(got) != 2 {
		t.Fatalf("counts=%v", got)
	}
	if current[chat.Codex] != 1 || current[chat.Agy] != 1 {
		t.Fatalf("input map was mutated: %v", current)
	}

	if _, err := parseWorkerCounts([]string{"/workers", "@codex", "2", "@claude", "nope"}, current); err == nil {
		t.Fatal("malformed second pair was accepted")
	}
	if current[chat.Codex] != 1 || current[chat.Agy] != 1 {
		t.Fatalf("failed update mutated input: %v", current)
	}
}

func TestParseWorkerCountsAllOffAndCaps(t *testing.T) {
	all, err := parseWorkerCounts([]string{"/workers", "@all", "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("all counts=%v", all)
	}
	for _, provider := range chat.Agents() {
		if all[provider] != 2 {
			t.Fatalf("%s count=%d", provider, all[provider])
		}
	}
	if _, err := parseWorkerCounts([]string{"/workers", "@all", "3"}, nil); err == nil {
		t.Fatal("total cap was not enforced")
	}
	off, err := parseWorkerCounts([]string{"/workers", "off"}, all)
	if err != nil || len(off) != 0 {
		t.Fatalf("off=%v err=%v", off, err)
	}
}

func TestParseWorkerCountsRejectsAuxiliaryAndDuplicateProviders(t *testing.T) {
	for _, fields := range [][]string{
		{"/workers", "@codex-1", "1"},
		{"/workers", "@codex", "1", "@codex", "2"},
		{"/workers", "@all", "1", "@codex", "2"},
		{"/workers", "@codex", "-1"},
	} {
		if _, err := parseWorkerCounts(fields, nil); err == nil {
			t.Fatalf("accepted %v", fields)
		}
	}
}

func TestParseDelegationRequiresAuxiliaryAndPreservesTask(t *testing.T) {
	participant, task, err := parseDelegation("/delegate @codex-2 inspect parser\nthen run its tests")
	if err != nil {
		t.Fatal(err)
	}
	if participant != chat.Participant("codex-2") || task != "inspect parser\nthen run its tests" {
		t.Fatalf("participant=%s task=%q", participant, task)
	}
	for _, value := range []string{"/delegate", "/delegate @codex task", "/delegate @codex-1", "/delegate @unknown-1 task"} {
		if _, _, err := parseDelegation(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestRosterAndStylesIncludeAuxiliaryProviders(t *testing.T) {
	participants := rosterParticipants([]chat.Participant{chat.Participant("claude-1"), chat.Codex, chat.Participant("codex-2")})
	joined := fmt.Sprint(participants)
	if !strings.Contains(joined, "codex-2") || !strings.Contains(joined, "claude-1") || len(participants) != 6 {
		t.Fatalf("participants=%v", participants)
	}
	if got, want := authorStyle(chat.Participant("codex-2")).Render("worker"), codexStyle.Render("worker"); got != want {
		t.Fatalf("auxiliary style=%q want %q", got, want)
	}
}

func TestActivityLineShowsEffectiveSettingsWithoutOrchestrator(t *testing.T) {
	model := Model{activity: map[chat.Participant]participantActivity{chat.Codex: {Phase: phaseIdle, Role: "lead", Task: "implement queue"}}, width: 100, progressMode: chat.ProgressDetailed}
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "lead") || !strings.Contains(line, "implement queue") {
		t.Fatalf("activity line=%q", line)
	}
}

func TestQuietActivityCollapsesBehindTheScenesDetail(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{
			chat.Codex: {Phase: phaseReading, Detail: "running a noisy command", StartedAt: time.Now().Add(-3 * time.Second)},
		},
		now: time.Now(), width: 100, progressMode: chat.ProgressCompact,
	}
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "reading") || strings.Contains(line, "noisy command") || strings.Contains(line, "workspace") {
		t.Fatalf("quiet activity line=%q", line)
	}
}

func TestProgressWorkboardShowsAssignmentQueueAndStalledState(t *testing.T) {
	now := time.Now()
	model := Model{
		room: chat.Room{PendingInputs: []uint64{8, 9}},
		activity: map[chat.Participant]participantActivity{
			chat.Codex: {Phase: phaseTesting, Role: "lead", Task: "implement queued input", Detail: "go test ./...", StartedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-70 * time.Second)},
		},
		now: now, width: 120, progressMode: chat.ProgressCompact,
	}
	board := model.activityView()
	for _, wanted := range []string{"lead", "testing", "implement queued input", "stalled?", "QUEUED 2", "/steer"} {
		if !strings.Contains(board, wanted) {
			t.Fatalf("compact workboard missing %q: %q", wanted, board)
		}
	}
	if strings.Contains(board, "go test ./...") {
		t.Fatalf("compact workboard leaked detail: %q", board)
	}
	model.progressMode = chat.ProgressDetailed
	if board := model.activityView(); !strings.Contains(board, "go test ./...") {
		t.Fatalf("detailed workboard missing safe detail: %q", board)
	}
	model.progressMode = chat.ProgressOff
	if board := model.activityView(); board != "" {
		t.Fatalf("off workboard=%q", board)
	}
}

func TestProgressCommandUpdatesPersonalMode(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore, rosterTestAgent{chat.Codex})
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	preferences := &workerTestPreferences{}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	model := New(orchestrator, roomStore)
	model.submit("/progress detailed")
	if model.progressMode != chat.ProgressDetailed || preferences.progress != chat.ProgressDetailed {
		t.Fatalf("model mode=%q preference=%q", model.progressMode, preferences.progress)
	}
	model.submit("/progress invalid")
	if preferences.progress != chat.ProgressDetailed {
		t.Fatalf("invalid command mutated preference=%q", preferences.progress)
	}
}

func TestDetailsToggleRevealsHistoricalToolMessages(t *testing.T) {
	model := Model{
		messages: []chat.Message{{Author: chat.Codex, Kind: chat.MessageTool, Text: "go test ./...", CreatedAt: time.Now()}},
		viewport: viewport.New(80, 20), ready: true, width: 80,
		activity: map[chat.Participant]participantActivity{}, live: map[chat.Participant]string{},
	}
	model.refreshContent()
	if strings.Contains(model.viewport.View(), "go test ./...") {
		t.Fatal("tool detail was visible in quiet mode")
	}
	model.showDetails = true
	model.refreshContent()
	if !strings.Contains(model.viewport.View(), "go test ./...") {
		t.Fatal("historical tool detail was not revealed")
	}
}

func TestConcurrentApprovalsAreQueued(t *testing.T) {
	model := Model{activity: map[chat.Participant]participantActivity{}, now: time.Now()}
	first := &agent.ApprovalRequest{Agent: chat.Codex, Title: "first", Response: make(chan agent.ApprovalDecision, 1)}
	second := &agent.ApprovalRequest{Agent: chat.Claude, Title: "second", Response: make(chan agent.ApprovalDecision, 1)}
	model.enqueueApproval(first)
	model.enqueueApproval(second)
	if model.pending != first || len(model.approvalQueue) != 1 {
		t.Fatalf("pending=%v queue=%v", model.pending, model.approvalQueue)
	}
	model.pending.Response <- agent.ApproveOnce
	model.pending = nil
	model.advanceApproval()
	if model.pending != second || len(model.approvalQueue) != 0 {
		t.Fatalf("pending=%v queue=%v", model.pending, model.approvalQueue)
	}
}

func TestJoinStatusMessageIsNotDuplicatedByImmediateRosterSync(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore,
		rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude}, rosterTestAgent{chat.Agy},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/join @agy")
	if len(model.messages) != 0 {
		t.Fatalf("metadata sync copied queued status message: %v", model.messages)
	}
	select {
	case event := <-orchestrator.Events():
		model.applyRoomEvent(event)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for join status")
	}
	if len(model.messages) != 1 || model.messages[0].Text != "agy joined the room" {
		t.Fatalf("messages=%v", model.messages)
	}
}

func TestModeratorCommandAndConfiguredRolePresentation(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members[chat.Agy] = true
	orchestrator, err := room.New(roomState, nil, roomStore,
		rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude}, rosterTestAgent{chat.Agy},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/moderator @claude")
	if orchestrator.Moderator() != chat.Claude {
		t.Fatalf("moderator=%s", orchestrator.Moderator())
	}
	if line := model.activityLine(chat.Claude); !strings.Contains(line, "◆ MOD") {
		t.Fatalf("moderator badge missing from activity line: %q", line)
	}
	if line := model.activityLine(chat.Codex); strings.Contains(line, "◆ MOD") {
		t.Fatalf("former moderator retained badge: %q", line)
	}
	model.notices = nil
	model.showAgents()
	var noticeText []string
	for _, notice := range model.notices {
		noticeText = append(noticeText, notice.Text)
	}
	joined := strings.Join(noticeText, "\n")
	if !strings.Contains(joined, "CLAUDE ◆ MOD") || !strings.Contains(joined, "preferred core, moderator") || !strings.Contains(joined, "AGY") || !strings.Contains(joined, "fallback peer") {
		t.Fatalf("agents output=%q", joined)
	}
	model.submit("/moderator auto")
	if status := orchestrator.CoreStatus(); status.ModeratorExplicit || status.ModeratorPreference != "" {
		t.Fatalf("automatic moderator status=%+v", status)
	}
}

func TestCoreCommandsPromoteFallbackAndAllowItToModerate(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true, chat.Agy: true, chat.Copilot: true}
	orchestrator, err := room.New(roomState, nil, roomStore,
		rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude}, rosterTestAgent{chat.Agy}, rosterTestAgent{chat.Copilot},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/core replace @claude @agy")
	model.submit("/moderator @agy")
	status := orchestrator.CoreStatus()
	if status.Moderator != chat.Agy || len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy || status.Promotions[0].Replaces != chat.Claude {
		t.Fatalf("core status=%+v", status)
	}
	model.notices = nil
	model.showAgents()
	joined := model.notices[len(model.notices)-1].Text
	if !strings.Contains(joined, "AGY ◆ MOD") || !strings.Contains(joined, "temporary core for claude, moderator") {
		t.Fatalf("agents output=%q", joined)
	}
}

func TestCoreUnavailableCommandParsesRetryAndTriggersAutomaticFallback(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members[chat.Agy] = true
	orchestrator, err := room.New(roomState, nil, roomStore,
		rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude}, rosterTestAgent{chat.Agy},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/core unavailable @claude for 2h provider session limit")
	status := orchestrator.CoreStatus()
	availability, ok := status.Availability[chat.Claude]
	if !ok || availability.RetryAt == nil || availability.Reason != "provider session limit" {
		t.Fatalf("availability=%+v", availability)
	}
	if len(status.Promotions) != 1 || status.Promotions[0].Participant != chat.Agy || status.Promotions[0].Replaces != chat.Claude {
		t.Fatalf("promotions=%+v", status.Promotions)
	}
}

func TestParseCoreAvailabilityRejectsPastAndAcceptsDuration(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	participant, availability, err := parseCoreAvailability([]string{"/core", "unavailable", "@claude", "30m", "quota"}, now)
	if err != nil || participant != chat.Claude || availability.RetryAt == nil || !availability.RetryAt.Equal(now.Add(30*time.Minute)) || availability.Reason != "quota" {
		t.Fatalf("participant=%s availability=%+v err=%v", participant, availability, err)
	}
	if _, _, err := parseCoreAvailability([]string{"/core", "unavailable", "@claude", "until", "2026-08-22T11:00:00Z"}, now); err == nil {
		t.Fatal("past retry time was accepted")
	}
}

func TestOptionalParticipantPermissionCommandIsAccepted(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := room.New(roomState, nil, roomStore,
		rosterTestAgent{chat.Codex}, rosterTestAgent{chat.Claude}, rosterTestAgent{chat.Agy},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer orchestrator.Close()
	model := New(orchestrator, roomStore)
	model.submit("/permissions @agy workspace")
	if got := orchestrator.EffectiveSettings()[chat.Agy].Permissions; got != chat.PermissionWorkspace {
		t.Fatalf("AGY permissions=%s", got)
	}
	if len(model.notices) == 0 || !strings.Contains(model.notices[len(model.notices)-1].Text, "Updated permissions for agy") {
		t.Fatalf("notices=%v", model.notices)
	}
}
