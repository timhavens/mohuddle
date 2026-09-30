package room

import (
	"errors"
	"github.com/timhavens/mohuddle/internal/chat"
	"testing"
)

func TestCloseForResetRefusesUnfinishedWorkAtomically(t *testing.T) {
	for _, state := range []chat.WorkflowState{chat.WorkflowQueued, chat.WorkflowActive, chat.WorkflowWaiting, chat.WorkflowNeedsAttention} {
		t.Run(string(state), func(t *testing.T) {
			o, _, _ := newTestOrchestrator(t)
			defer o.Close()
			o.mu.Lock()
			o.room.Workflows["pending"] = chat.WorkflowRecord{State: state}
			o.mu.Unlock()
			if err := o.CloseForReset(); !errors.Is(err, ErrResetBusy) {
				t.Fatalf("reset allowed: %v", err)
			}
			o.mu.Lock()
			closed := o.closed
			delete(o.room.Workflows, "pending")
			o.mu.Unlock()
			if closed {
				t.Fatal("refused reset closed the room")
			}
			if err := o.CloseForReset(); err != nil {
				t.Fatal(err)
			}
			if err := o.Post("must not run after reset"); err == nil {
				t.Fatal("closed room accepted work")
			}
		})
	}
}
