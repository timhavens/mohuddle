package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

func TestPublicLiveTextHidesControlMarker(t *testing.T) {
	value := "public response\n<!-- mohuddle:{\"done\":true} -->"
	if got := publicLiveText(value); got != "public response\n" {
		t.Fatalf("got %q", got)
	}
}

func TestActivityTracksSilentWorkToolsAndCompletion(t *testing.T) {
	started := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	model := Model{
		activity: map[chat.Participant]participantActivity{},
		live:     map[chat.Participant]string{},
		now:      started,
		width:    100,
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

func TestParseSettingsChangeSupportsDefaultsAndBothAgents(t *testing.T) {
	change, err := parseSettingsChange("/permissions", []string{"/permissions", "default", "@all", "full"})
	if err != nil {
		t.Fatal(err)
	}
	if !change.Default || change.Field != "permissions" || change.Value != "full" || len(change.Participants) != 2 {
		t.Fatalf("change=%+v", change)
	}
	if _, err := parseSettingsChange("/permissions", []string{"/permissions", "@codex", "unknown"}); err == nil {
		t.Fatal("invalid permission profile was accepted")
	}
}

func TestActivityLineShowsEffectiveSettingsWithoutOrchestrator(t *testing.T) {
	model := Model{activity: map[chat.Participant]participantActivity{chat.Codex: {Phase: phaseIdle}}, width: 100}
	line := model.activityLine(chat.Codex)
	if !strings.Contains(line, "default · auto · workspace") {
		t.Fatalf("activity line=%q", line)
	}
}
