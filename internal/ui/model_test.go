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
	"github.com/timhavens/mohuddle/internal/store"
)

type rosterTestAgent struct{ participant chat.Participant }

func (a rosterTestAgent) Participant() chat.Participant { return a.participant }
func (a rosterTestAgent) Close() error                  { return nil }
func (a rosterTestAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: "done", Done: true}, nil
}

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
