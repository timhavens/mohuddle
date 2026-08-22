package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"

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

func TestModeratorCommandAndVoiceRolePresentation(t *testing.T) {
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
	joined := strings.Join(model.notices, "\n")
	if !strings.Contains(joined, "CLAUDE ◆ MOD") || !strings.Contains(joined, "core-worker, moderator") || !strings.Contains(joined, "AGY") || !strings.Contains(joined, "voice") {
		t.Fatalf("agents output=%q", joined)
	}
}

func TestVoicePermissionCommandIsRejected(t *testing.T) {
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
	if len(model.notices) == 0 || !strings.Contains(model.notices[len(model.notices)-1], "permanently voice-only") {
		t.Fatalf("notices=%v", model.notices)
	}
}
