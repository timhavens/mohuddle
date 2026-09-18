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

func TestToolChoiceGuidanceReachesEveryProviderAndWorker(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	participants := append(chat.Agents(), "codex-1", "claude-1", "agy-1", "copilot-1")
	for _, participant := range participants {
		t.Run(string(participant), func(t *testing.T) {
			for _, mode := range []string{"default", "custom", "resumed", "delegated", "plan"} {
				t.Run(mode, func(t *testing.T) {
					spec := turnSpec{through: 2, coreParticipants: []chat.Participant{participant}}
					o.mu.Lock()
					o.messages = []chat.Message{
						{Sequence: 1, Author: chat.User, Text: "earlier request"},
						{Sequence: 2, Author: chat.User, Text: "current request"},
					}
					o.room.RoomPrompt = ""
					o.room.AgentPrompts = nil
					session := o.room.Sessions[participant]
					session.Cursor = 0
					switch mode {
					case "custom":
						o.room.RoomPrompt = "Room preference."
						o.room.AgentPrompts = map[chat.Participant]string{participant: "Individual preference."}
					case "resumed":
						session.Cursor = 1
						spec.after = 1
					case "delegated":
						spec.delegated, spec.readOnly = true, true
					case "plan":
						spec.planOnly, spec.readOnly = true, true
					}
					o.room.Sessions[participant] = session
					o.mu.Unlock()
					request := o.turnRequest(participant, spec, nil)
					if request.VoiceOnly || request.NoTools {
						t.Fatal("expected a turn with tool access")
					}
					for name, prompt := range map[string]string{"system": request.SystemPrompt, "turn input": request.Prompt} {
						if strings.Count(prompt, agent.ToolChoiceGuidance) != 1 {
							t.Fatalf("%s must receive the shared guidance exactly once", name)
						}
					}
					guidanceAt := strings.Index(request.Prompt, agent.ToolChoiceGuidance)
					transcriptAt := strings.Index(request.Prompt, "BEGIN UNTRUSTED ROOM TRANSCRIPT")
					if transcriptAt < 0 || guidanceAt >= transcriptAt {
						t.Fatal("tool guidance must be outside the untrusted transcript")
					}
					if mode == "resumed" && strings.Contains(request.Prompt, "earlier request") != request.Ephemeral {
						t.Fatal("fresh provider turns need earlier context; resumed native sessions must not replay it")
					}
					if mode == "custom" && request.PromptOverride != "Individual preference." {
						t.Fatal("shared guidance must preserve the human's prompt override")
					}
					if spec.readOnly && (request.Settings.Permissions != chat.PermissionReadOnly || len(request.WriteRoots) != 0) {
						t.Fatal("tool guidance must preserve read-only permissions")
					}
				})
			}
		})
	}
}

func TestToolChoiceGuidanceOmittedWithoutToolAccess(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	for _, participant := range chat.Agents() {
		for _, mode := range []string{"routing", "no-tools", "isolated"} {
			t.Run(string(participant)+"/"+mode, func(t *testing.T) {
				spec := turnSpec{coreParticipants: []chat.Participant{participant}}
				switch mode {
				case "routing":
					spec.private, spec.ephemeral = true, true
				case "no-tools":
					spec.noTools = true
				case "isolated":
					spec.coreParticipants, spec.readOnly = nil, true
				}
				request := o.turnRequest(participant, spec, nil)
				if !request.VoiceOnly && !request.NoTools {
					t.Fatal("expected a turn without tool access")
				}
				if strings.Contains(request.SystemPrompt, agent.ToolChoiceGuidance) || strings.Contains(request.Prompt, agent.ToolChoiceGuidance) {
					t.Fatal("turn without tools received tool-use instructions")
				}
				if len(request.ReadRoots) != 0 || len(request.WriteRoots) != 0 {
					t.Fatal("turn without tools received workspace roots")
				}
			})
		}
	}
}

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
