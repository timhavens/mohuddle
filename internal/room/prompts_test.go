package room

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

func TestPromptSettingsPrecedencePersistenceAndIsolation(t *testing.T) {
	o, codex, _ := newTestOrchestrator(t)
	defer o.Close()
	if err := o.SetPrompt(chat.System, "Room guidance\n  preserve whitespace"); err != nil {
		t.Fatal(err)
	}
	if err := o.SetPrompt(chat.Codex, "Codex override"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		participant chat.Participant
		want        string
	}{
		{chat.Codex, "Codex override"}, {chat.Claude, "Room guidance\n  preserve whitespace"}, {chat.Agy, "Room guidance\n  preserve whitespace"}, {chat.Copilot, "Room guidance\n  preserve whitespace"},
	} {
		preview, err := o.Prompt(tc.participant, true)
		if err != nil || !preview.Preview || preview.Request.PromptOverride != tc.want || !strings.Contains(preview.Request.Prompt, tc.want) {
			t.Fatalf("preview for %s: %+v, %v", tc.participant, preview, err)
		}
	}
	request := o.turnRequest(chat.Codex, turnSpec{private: true, ephemeral: true}, nil)
	if request.PromptOverride != "" || strings.Contains(request.Prompt, "Codex override") {
		t.Fatal("custom prompt influenced private routing")
	}
	state, messages := o.Snapshot()
	persisted, err := o.store.(*store.Store).LoadRoom(state.ID)
	if err != nil || persisted.RoomPrompt != state.RoomPrompt || persisted.AgentPrompts[chat.Codex] != "Codex override" {
		t.Fatalf("saved prompt settings: %+v, %v", persisted, err)
	}
	state.AgentPrompts[chat.Codex] = "mutated snapshot"
	state, _ = o.Snapshot()
	if state.AgentPrompts[chat.Codex] != "Codex override" || len(messages) != 0 || codex.callCount() != 0 {
		t.Fatal("prompt commands changed provider state, history, or shared configuration")
	}
	if err := o.SetPrompt(chat.Codex, ""); err != nil {
		t.Fatal(err)
	}
	preview, _ := o.Prompt(chat.Codex, true)
	if preview.Request.PromptOverride != state.RoomPrompt {
		t.Fatal("clearing individual override did not restore room inheritance")
	}
	if err := o.SetPrompt(chat.System, ""); err != nil {
		t.Fatal(err)
	}
	preview, _ = o.Prompt(chat.Codex, true)
	if preview.Request.PromptOverride != "" || !strings.Contains(preview.Request.Prompt, "Discard any earlier") {
		t.Fatal("clearing room prompt did not restore native defaults")
	}
}

type failingPromptStore struct{ Store }

func (s failingPromptStore) SaveRoom(chat.Room) error { return errors.New("disk full") }

func TestPromptSettingsRejectInvalidAndFailedWrites(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	for _, tc := range []struct {
		participant chat.Participant
		text        string
	}{
		{chat.User, "invalid target"}, {chat.Participant("codex-99"), "unknown worker"},
		{chat.Codex, strings.Repeat("x", MaxCustomPromptBytes+1)}, {chat.Codex, "bad\x00text"},
		{chat.Codex, string([]byte{0xff})},
	} {
		if err := o.SetPrompt(tc.participant, tc.text); err == nil {
			t.Fatalf("accepted invalid prompt for %s", tc.participant)
		}
	}
	if err := o.SetPrompt(chat.System, "original"); err != nil {
		t.Fatal(err)
	}
	o.store = failingPromptStore{o.store}
	if err := o.SetPrompt(chat.System, "unsaved"); err == nil {
		t.Fatal("save failure was hidden")
	}
	state, _ := o.Snapshot()
	if state.RoomPrompt != "original" {
		t.Fatal("failed save changed the running configuration")
	}
}

func TestPromptCaptureRemainsExactWhileSettingsChange(t *testing.T) {
	o, codex, _ := newTestOrchestrator(t)
	defer o.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	codex.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		select {
		case <-release:
			return agent.TurnResult{Text: "done", Done: true}, nil
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
	}
	if err := o.SetPrompt(chat.System, "first guidance"); err != nil {
		t.Fatal(err)
	}
	if err := o.Post("@codex inspect this request"); err != nil {
		t.Fatal(err)
	}
	<-started
	capture, err := o.Prompt(chat.Codex, false)
	if err != nil || capture.Preview || !capture.Active || capture.CapturedAt.IsZero() || !reflect.DeepEqual(capture.Request, codex.request(0)) {
		t.Fatalf("in-flight capture: %+v, %v", capture, err)
	}
	if err := o.SetPrompt(chat.System, "next guidance"); err != nil {
		t.Fatal(err)
	}
	preview, _ := o.Prompt(chat.Codex, true)
	latest, _ := o.Prompt(chat.Codex, false)
	if preview.Request.PromptOverride != "next guidance" || latest.Request.PromptOverride != "first guidance" {
		t.Fatal("preview was confused with a captured provider request")
	}
	if len(latest.Request.ReadRoots) > 0 {
		latest.Request.ReadRoots[0] = "changed copy"
	}
	latest, _ = o.Prompt(chat.Codex, false)
	if !reflect.DeepEqual(capture.Request, latest.Request) {
		t.Fatal("mutating a returned snapshot changed the stored capture")
	}
	close(release)
	waitForRound(t, o.Events(), nil)
	latest, _ = o.Prompt(chat.Codex, false)
	if latest.Active || latest.Request.PromptOverride != "first guidance" {
		t.Fatal("completed capture was not retained accurately")
	}
}

func TestPromptChangesRefreshNativeSessionWithinWorkflow(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	codex.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.NoTools {
			return bidResult(chat.Codex, chat.Codex), nil
		}
		if request.PromptOverride == "first" {
			if err := o.SetPrompt(chat.Codex, "second"); err != nil {
				t.Error(err)
			}
		}
		return agent.TurnResult{Text: "done", SessionID: "native-session", Done: true}, nil
	}
	claude.run = func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		if request.NoTools {
			return bidResult(chat.Claude, chat.Codex), nil
		}
		return agent.TurnResult{Done: true}, nil
	}
	if err := o.SetPrompt(chat.Codex, "first"); err != nil {
		t.Fatal(err)
	}
	if err := o.Post("implement the requested change"); err != nil {
		t.Fatal(err)
	}
	waitForRound(t, o.Events(), nil)
	latest, _ := o.Prompt(chat.Codex, false)
	if codex.resetCount() < 2 || latest.Request.PromptOverride != "second" {
		t.Fatalf("prompt change kept stale native context: resets=%d override=%q", codex.resetCount(), latest.Request.PromptOverride)
	}
	state, _ := o.Snapshot()
	if state.Sessions[chat.Codex].PromptHash != customPromptHash("second") {
		t.Fatal("session prompt revision was not persisted")
	}
}
