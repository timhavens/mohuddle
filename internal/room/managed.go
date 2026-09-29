package room

import (
	"fmt"
	"sync"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

// SubscribeView replays still-pending human approvals when a background room
// becomes visible. The manager continuously drains the primary event stream.
func (o *Orchestrator) SubscribeView() (<-chan Event, func()) {
	o.eventMu.Lock()
	defer o.eventMu.Unlock()
	stream := make(chan Event, 512+len(o.pendingApprovals))
	for _, event := range o.pendingApprovals {
		stream <- event
	}
	o.nextSubscriber++
	id := o.nextSubscriber
	o.subscribers[id] = &eventSubscriber{stream: stream}
	var once sync.Once
	return stream, func() {
		once.Do(func() {
			o.eventMu.Lock()
			defer o.eventMu.Unlock()
			if _, ok := o.subscribers[id]; ok {
				delete(o.subscribers, id)
				close(stream)
			}
		})
	}
}

func (o *Orchestrator) ForgetApproval(request *agent.ApprovalRequest) {
	o.eventMu.Lock()
	delete(o.pendingApprovals, request)
	o.eventMu.Unlock()
}

// ReplaceAgents preserves the room, grants, and sessions during an idle worker
// topology refresh. Callers hold their manager dispatch barrier.
func (o *Orchestrator) ReplaceAgents(agents []agent.Agent, members map[chat.Participant]bool) error {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	if o.activeWork > 0 || len(o.activeTurns) > 0 {
		o.mu.Unlock()
		for _, a := range agents {
			_ = a.Close()
		}
		return fmt.Errorf("worker topology cannot change while work is active")
	}
	old := o.agents
	copy := cloneRoom(o.room)
	copy.Members = members
	if err := o.store.SaveRoom(copy); err != nil {
		o.mu.Unlock()
		for _, a := range agents {
			_ = a.Close()
		}
		return err
	}
	o.agents = map[chat.Participant]agent.Agent{}
	for _, a := range agents {
		p := a.Participant()
		o.agents[p] = a
		if o.agentGates[p] == nil {
			o.agentGates[p] = &sync.Mutex{}
		}
	}
	o.room.Members = members
	o.mu.Unlock()
	for _, a := range old {
		_ = a.Close()
	}
	return nil
}
