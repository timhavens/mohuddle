package room

import (
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

// TurnPreview is a bounded, replaceable display snapshot. Final answers and
// retained drafts are assembled separately; UI speed never controls capture.
type TurnPreview struct {
	Participant chat.Participant
	TurnID      string
	Text        string
	Truncated   bool
}

func (o *Orchestrator) PreviewUpdates() <-chan struct{} { return o.previewWake }

func (o *Orchestrator) updatePreview(event Event) {
	o.previewMu.Lock()
	defer o.previewMu.Unlock()
	if o.previews == nil {
		o.previews = make(map[chat.Participant]TurnPreview)
	}
	p := o.previews[event.Participant]
	if p.TurnID != event.TurnID {
		p = TurnPreview{Participant: event.Participant, TurnID: event.TurnID}
	}
	if event.AgentEvent.Type == agent.EventReset {
		p.Text = ""
		p.Truncated = false
	} else {
		remaining := maxTurnDraftBytes - len(p.Text)
		text := truncateUTF8Prefix(event.AgentEvent.Text, remaining)
		p.Text += text
		p.Truncated = p.Truncated || len(text) < len(event.AgentEvent.Text)
		o.previewUpdates++
	}
	o.previews[event.Participant] = p
	select {
	case o.previewWake <- struct{}{}:
	default:
		o.previewCoalesced++
	}
}

// Previews does not copy the room or transcript. It is read only on a bounded
// UI tick, not once for every streamed token.
func (o *Orchestrator) Previews() []TurnPreview {
	o.previewMu.Lock()
	defer o.previewMu.Unlock()
	values := make([]TurnPreview, 0, len(o.previews))
	for _, p := range o.previews {
		p.Text = agent.SanitizeResponseDraft(p.Text)
		values = append(values, p)
	}
	return values
}

func (o *Orchestrator) finishPreview(event Event) {
	o.previewMu.Lock()
	defer o.previewMu.Unlock()
	if p, ok := o.previews[event.Participant]; ok && p.TurnID == event.TurnID {
		delete(o.previews, event.Participant)
	}
}
