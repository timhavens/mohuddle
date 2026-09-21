package codex

import (
	"encoding/json"
	"sync"

	"github.com/timhavens/mohuddle/internal/agent"
)

const maxQueuedEvents = 4096
const maxQueuedBytes = 16 * 1024 * 1024

// RPC responses bypass this queue. The reader never waits for the turn consumer,
// which may itself be awaiting an RPC response or a human approval.
type notificationQueue struct {
	mu                     sync.Mutex
	ready                  chan struct{}
	items                  []rpcMessage
	head, bytes            int
	err                    error
	highEntries, highBytes int
	coalesced, overflows   uint64
	ended                  error
}

func newNotificationQueue() *notificationQueue {
	return &notificationQueue{ready: make(chan struct{}, 1)}
}

func (q *notificationQueue) wake() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func eventBytes(m rpcMessage) int { return len(m.ID) + len(m.Method) + len(m.Params) + len(m.Result) }

type textDelta struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

func (q *notificationQueue) push(m rpcMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	added := eventBytes(m)
	merged := false
	if len(m.ID) == 0 && m.Method == "item/agentMessage/delta" && len(q.items) > q.head {
		last := &q.items[len(q.items)-1]
		var a, b textDelta
		if len(last.ID) == 0 && last.Method == m.Method && json.Unmarshal(last.Params, &a) == nil && json.Unmarshal(m.Params, &b) == nil && a.ThreadID == b.ThreadID && a.TurnID == b.TurnID && a.ItemID == b.ItemID && a.ThreadID != "" && a.TurnID != "" && a.ItemID != "" && len(a.Delta)+len(b.Delta) <= 64*1024 {
			a.Delta += b.Delta
			encoded, _ := json.Marshal(a)
			added = len(encoded) - len(last.Params)
			if q.bytes+added <= maxQueuedBytes {
				last.Params = encoded
				merged = true
				q.coalesced++
			}
		}
	}
	if q.bytes+added > maxQueuedBytes || (!merged && len(q.items)-q.head >= maxQueuedEvents) {
		q.err = &agent.EventQueueOverflowError{}
		q.overflows++
		q.wake()
		return q.err
	}
	if !merged {
		q.items = append(q.items, m)
	}
	q.bytes += added
	q.highEntries = max(q.highEntries, len(q.items)-q.head)
	q.highBytes = max(q.highBytes, q.bytes)
	q.wake()
	return nil
}

func (q *notificationQueue) pop() (rpcMessage, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return rpcMessage{}, false, q.err
	}
	if q.head == len(q.items) {
		return rpcMessage{}, false, q.ended
	}
	m := q.items[q.head]
	q.items[q.head] = rpcMessage{}
	q.head++
	q.bytes -= eventBytes(m)
	if q.head == len(q.items) {
		q.items = nil
		q.head = 0
		if q.ended != nil {
			q.wake()
		}
	} else {
		if q.head >= 1024 {
			q.items = append([]rpcMessage(nil), q.items[q.head:]...)
			q.head = 0
		}
		q.wake()
	}
	return m, true, nil
}

func (q *notificationQueue) fail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err == nil {
		q.err = err
	}
	q.wake()
}

func consumedNotification(method string) bool {
	switch method {
	case "thread/settings/updated", "item/agentMessage/delta", "item/started", "item/completed", "turn/completed", "error":
		return true
	}
	return false
}

func (q *notificationQueue) end(err error) { q.mu.Lock(); defer q.mu.Unlock(); q.ended = err; q.wake() }
