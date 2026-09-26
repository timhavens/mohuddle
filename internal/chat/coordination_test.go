package chat

import (
	"testing"
	"time"
)

func TestCoordinationWarningsUseProgressNotAcknowledgements(t *testing.T) {
	now := time.Now().UTC()
	r := &CoordinationRun{ID: "run", State: "pending", StartedAt: now}
	if r.View(now.Add(179*time.Second), 0, 0).ActionNeeded {
		t.Fatal("warned before threshold")
	}
	r.AcknowledgedAt = now.Add(179 * time.Second)
	if !r.View(now.Add(180*time.Second), 0, 0).ActionNeeded {
		t.Fatal("acknowledgement hid idle time")
	}
	if r.View(now.Add(time.Hour), 1, 1).ActionNeeded {
		t.Fatal("resource wait treated as idle")
	}
	if !r.View(now.Add(time.Hour), 1, 1).NoCompletion {
		t.Fatal("missing no-completion diagnostic")
	}
	r.LastResultAt = now.Add(58 * time.Minute)
	if r.View(now.Add(time.Hour), 0, 0).ActionNeeded {
		t.Fatal("new result did not start a fresh handoff window")
	}
	if !r.View(now.Add(time.Hour), 0, 0).NoCompletion {
		t.Fatal("failed result counted as success")
	}
	r.LastSuccessAt = r.LastResultAt
	if r.View(now.Add(time.Hour), 0, 0).NoCompletion {
		t.Fatal("successful operation not reflected")
	}
	for _, state := range []string{"blocked", "complete", "stopped"} {
		r.State = state
		v := r.View(now.Add(24*time.Hour), 0, 0)
		if v.ActionNeeded || v.NoCompletion {
			t.Fatalf("warned for %s", state)
		}
	}
}

func TestCoordinationHistoryIsBoundedAndCloned(t *testing.T) {
	r := &CoordinationRun{State: "pending", StartedAt: time.Now()}
	for i := 0; i < 250; i++ {
		r.Record(CoordinationEvent{ID: time.Unix(int64(i), 0).String(), Kind: "result_available"})
	}
	if len(r.Events) != 200 {
		t.Fatal(len(r.Events))
	}
	c := r.Clone()
	c.Events[0].Kind = "changed"
	if r.Events[0].Kind == "changed" {
		t.Fatal("aliased persisted history")
	}
	v := r.View(time.Now(), 0, 0)
	if len(v.Events) != 20 {
		t.Fatal("view is not bounded")
	}
}
