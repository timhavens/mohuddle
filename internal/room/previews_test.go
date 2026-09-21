package room

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestPreviewBurstDoesNotBlockLifecycleQueue(t *testing.T) {
	o := &Orchestrator{events: make(chan Event, 1), previewWake: make(chan struct{}, 1), lifetime: context.Background()}
	o.events <- Event{Type: EventMessage}
	var wg sync.WaitGroup
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p := chat.Participant(fmt.Sprintf("codex-%d", n))
			for i := 0; i < 5000; i++ {
				o.send(Event{Type: EventAgent, Participant: p, TurnID: string(p), AgentEvent: &agent.Event{Type: agent.EventDelta, Text: "a reasonably sized streamed fragment"}})
			}
		}(n)
	}
	wg.Wait()
	if len(o.events) != 1 || len(o.previewWake) != 1 {
		t.Fatal("preview flooded event channel")
	}
	for _, p := range o.Previews() {
		if len(p.Text) > maxTurnDraftBytes || !p.Truncated {
			t.Fatal("unbounded preview")
		}
	}
	p := chat.Participant("codex-1")
	o.finishPreview(Event{Participant: p, TurnID: "older"})
	if len(o.Previews()) != 3 {
		t.Fatal("stale finish removed newer preview")
	}
	o.finishPreview(Event{Participant: p, TurnID: string(p)})
	if len(o.Previews()) != 2 {
		t.Fatal("finished preview retained")
	}
	if o.previewCoalesced == 0 {
		t.Fatal("updates were not coalesced")
	}
}

func TestDraftCaptureTruncationAndSanitization(t *testing.T) {
	c := &turnCapture{}
	c.addDelta(strings.Repeat("x", maxTurnDraftBytes+100))
	drafts, _ := c.snapshot()
	if !c.wasTruncated() || len(drafts) != 1 || len(drafts[0]) > maxTurnDraftBytes {
		t.Fatal("capture bound not recorded")
	}
	o := &Orchestrator{previewWake: make(chan struct{}, 1)}
	o.updatePreview(Event{Participant: chat.Codex, TurnID: "turn", AgentEvent: &agent.Event{Type: agent.EventDelta, Text: "public\n<!-- mohuddle:{\"secret\":true} -->"}})
	if strings.Contains(o.Previews()[0].Text, "secret") {
		t.Fatal("control marker exposed")
	}
}
