package ui

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/speech"
	"github.com/timhavens/mohuddle/internal/store"
)

type rosterTestAgent struct{ participant chat.Participant }

func (a rosterTestAgent) Participant() chat.Participant { return a.participant }
func (a rosterTestAgent) Close() error                  { return nil }
func (a rosterTestAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: "done", Done: true}, nil
}

type spokenMessage struct {
	agent chat.Participant
	text  string
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
		activity:    map[chat.Participant]participantActivity{},
		live:        map[chat.Participant]string{},
		now:         started,
		width:       100,
		showDetails: true,
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
	if !strings.Contains(line, "using tool") || !strings.Contains(line, "5s") || !strings.Contains(line, "go test ./...") {
		t.Fatalf("activity line=%q", line)
	}

	model.applyRoomEvent(room.Event{Type: room.EventTurnFinished, Participant: chat.Codex})
	activity := model.activity[chat.Codex]
	if activity.Phase != phaseIdle || activity.Detail != "running go test ./..." || !activity.StartedAt.IsZero() {
		t.Fatalf("finished activity=%+v", activity)
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

func TestActivityLineShowsEffectiveSettingsWithoutOrchestrator(t *testing.T) {
	model := Model{activity: map[chat.Participant]participantActivity{chat.Codex: {Phase: phaseIdle}}, width: 100, showDetails: true}
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "default · auto · workspace") {
		t.Fatalf("activity line=%q", line)
	}
}

func TestQuietActivityCollapsesBehindTheScenesDetail(t *testing.T) {
	model := Model{
		activity: map[chat.Participant]participantActivity{
			chat.Codex: {Phase: phaseTool, Detail: "running a noisy command", StartedAt: time.Now().Add(-3 * time.Second)},
		},
		now: time.Now(), width: 100,
	}
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "working") || strings.Contains(line, "noisy command") || strings.Contains(line, "workspace") {
		t.Fatalf("quiet activity line=%q", line)
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
