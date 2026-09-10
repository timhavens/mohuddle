package room

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

const MaxCustomPromptBytes = 32 * 1024

// SetPrompt saves the room default (participant == chat.System) or one exact
// participant's override. Empty text clears that scope. In-flight requests keep
// their captured guidance; later requests select the newly saved value.
func (o *Orchestrator) SetPrompt(participant chat.Participant, text string) error {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return fmt.Errorf("prompt must be valid text without NUL characters")
	}
	if len(text) > MaxCustomPromptBytes {
		return fmt.Errorf("prompt must be at most %d bytes", MaxCustomPromptBytes)
	}
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return fmt.Errorf("room is closed")
	}
	if participant != chat.System && !containsParticipant(o.settingsParticipantsLocked(), participant) {
		return fmt.Errorf("unknown room agent %q; configure auxiliary agents with /workers first", participant)
	}
	if _, temporary := o.temporary[participant]; temporary {
		return fmt.Errorf("temporary responders inherit the room prompt; choose a stable room agent for an override")
	}
	next := cloneRoom(o.room)
	if participant == chat.System {
		next.RoomPrompt = text
	} else if text == "" {
		delete(next.AgentPrompts, participant)
	} else {
		if next.AgentPrompts == nil {
			next.AgentPrompts = make(map[chat.Participant]string)
		}
		next.AgentPrompts[participant] = text
	}
	// Publish the configuration only after persistence succeeds, so a failed
	// save cannot briefly influence a concurrently starting provider request.
	if err := o.store.SaveRoom(next); err != nil {
		return fmt.Errorf("save prompt: %w", err)
	}
	o.room.RoomPrompt = next.RoomPrompt
	o.room.AgentPrompts = next.AgentPrompts
	return nil
}

type customPromptSelection struct {
	Text   string
	Source string
}

func selectCustomPrompt(roomState chat.Room, participant chat.Participant) customPromptSelection {
	selection := customPromptSelection{Text: roomState.RoomPrompt, Source: "room prompt"}
	if override := roomState.AgentPrompts[participant]; strings.TrimSpace(override) != "" {
		selection.Text = override
		selection.Source = "individual override for @" + string(participant) + " (replaces the room prompt)"
	}
	return selection
}

func customPromptHash(text string) string {
	if text == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func customPromptDirective(selection customPromptSelection) string {
	const header = "HOST-CONFIGURED PROMPT FOR THIS TURN:\n"
	if strings.TrimSpace(selection.Text) == "" {
		return header + "No custom prompt is configured. Discard any earlier room or individual custom guidance and use MoHuddle's default guidance for this turn."
	}
	return header + "The human selected the following guidance for this turn. It replaces earlier custom prompts and default response preferences. Keep the host-assigned identity, current workflow role, permissions, and required private response markers.\nSource: " + selection.Source + ".\n\n" + selection.Text + "\n\nEND OF CURRENT CUSTOM PROMPT."
}

// PromptSnapshot contains MoHuddle's input to a provider adapter, not the
// provider's native instructions or accumulated session context. Snapshots stay
// in memory, outside the shared transcript, room persistence, and event stream.
type PromptSnapshot struct {
	Participant    chat.Participant
	Request        agent.TurnRequest
	CapturedAt     time.Time
	TurnID         string
	WorkflowID     string
	ConversationID string
	Active         bool
	Preview        bool
}

// Prompt returns the latest public turn request for a participant. An explicit
// preview, or a participant with no captured turn in this process, uses the
// same builder as real turns without starting work or advancing session state.
// A preview has no assigned workflow role and is not a prediction of a turn.
func (o *Orchestrator) Prompt(participant chat.Participant, preview bool) (PromptSnapshot, error) {
	o.mu.Lock()
	if !containsParticipant(o.settingsParticipantsLocked(), participant) {
		o.mu.Unlock()
		return PromptSnapshot{}, fmt.Errorf("unknown room agent %q", participant)
	}
	if snapshot, ok := o.prompts[participant]; ok && !preview {
		active, running := o.activeTurns[participant]
		snapshot.Active = running && active.turnID == snapshot.TurnID
		snapshot.Request = clonePromptRequest(snapshot.Request)
		o.mu.Unlock()
		return snapshot, nil
	}
	spec := withWorkflowMode(turnSpec{
		after:            o.room.Sessions[participant].Cursor,
		coreParticipants: o.activeCoreParticipantsLocked(time.Now()),
	}, o.room.WorkflowMode)
	if len(o.messages) > 0 {
		spec.through = o.messages[len(o.messages)-1].Sequence
	}
	o.mu.Unlock()
	return PromptSnapshot{
		Participant: participant,
		Request:     clonePromptRequest(o.turnRequest(participant, spec, nil)),
		Preview:     true,
	}, nil
}

// capturePrompt runs immediately before dispatch. Callers exclude private
// routing bids so they cannot replace the human's current work or chat prompt.
func (o *Orchestrator) capturePrompt(participant chat.Participant, request agent.TurnRequest) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.prompts == nil {
		o.prompts = make(map[chat.Participant]PromptSnapshot)
	}
	active := o.activeTurns[participant]
	o.prompts[participant] = PromptSnapshot{
		Participant:    participant,
		Request:        clonePromptRequest(request),
		CapturedAt:     time.Now().UTC(),
		TurnID:         active.turnID,
		WorkflowID:     active.workflowID,
		ConversationID: active.conversationID,
	}
}

func clonePromptRequest(request agent.TurnRequest) agent.TurnRequest {
	request.Attachments = append([]chat.Attachment(nil), request.Attachments...)
	request.ReadRoots = append([]string(nil), request.ReadRoots...)
	request.WriteRoots = append([]string(nil), request.WriteRoots...)
	return request
}
