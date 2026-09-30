package room

import (
	"context"
	"fmt"
	"sync"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type sharedTicket struct {
	room  string
	ready chan struct{}
}
type sharedLane struct {
	active int
	limit  int
	last   string
	queue  []*sharedTicket
}

// SharedCapacity limits provider calls and checkout writers across room
// runtimes. Provider sessions and transcript context never enter this object.
type SharedCapacity struct {
	roomNames     map[string]string
	recovery      map[string]*chat.WorkspaceWriter
	saveJournal   func([]byte) error
	journalErr    error
	records       map[string]*chat.WorkspaceWriter
	lastWriters   map[string]*chat.WorkspaceWriter
	revisions     map[string]uint64
	writerTurns   map[string]int
	writerClosing map[string]bool
	mu            sync.Mutex
	lanes         map[chat.Participant]*sharedLane
	writers       map[string]string
	writerQueue   map[string][]string
	waiters       map[string]int
	changed       chan struct{}
}

func NewSharedCapacity() *SharedCapacity {
	return &SharedCapacity{roomNames: map[string]string{}, recovery: map[string]*chat.WorkspaceWriter{}, records: map[string]*chat.WorkspaceWriter{}, lastWriters: map[string]*chat.WorkspaceWriter{}, revisions: map[string]uint64{}, writerTurns: map[string]int{}, writerClosing: map[string]bool{}, lanes: map[chat.Participant]*sharedLane{}, writers: map[string]string{}, writerQueue: map[string][]string{}, waiters: map[string]int{}, changed: make(chan struct{})}
}
func (s *SharedCapacity) wake() { close(s.changed); s.changed = make(chan struct{}) }
func (s *SharedCapacity) dispatch(l *sharedLane) {
	for l.active < l.limit && len(l.queue) > 0 {
		index := 0
		for i, t := range l.queue {
			if t.room != l.last {
				index = i
				break
			}
		}
		t := l.queue[index]
		l.queue = append(l.queue[:index], l.queue[index+1:]...)
		l.active++
		l.last = t.room
		close(t.ready)
	}
}
func (s *SharedCapacity) setProviderLimit(provider chat.Participant, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lanes[provider]
	if l == nil {
		l = &sharedLane{}
		s.lanes[provider] = l
	}
	l.limit = max(1, limit)
	s.dispatch(l)
}
func (s *SharedCapacity) acquireProvider(ctx context.Context, room string, provider chat.Participant, limit int, waiting func()) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	l := s.lanes[provider]
	if l == nil {
		l = &sharedLane{limit: max(1, limit)}
		s.lanes[provider] = l
	}
	t := &sharedTicket{room: room, ready: make(chan struct{})}
	l.queue = append(l.queue, t)
	s.dispatch(l)
	queued := true
	select {
	case <-t.ready:
		queued = false
	default:
	}
	s.mu.Unlock()
	if queued {
		waiting()
	}
	select {
	case <-ctx.Done():
		s.mu.Lock()
		found := false
		for i, c := range l.queue {
			if c == t {
				l.queue = append(l.queue[:i], l.queue[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			l.active--
		}
		s.dispatch(l)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-t.ready:
	}
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); l.active--; s.dispatch(l); s.mu.Unlock() }) }, nil
}
func (s *SharedCapacity) acquireWriter(ctx context.Context, workspace, key string, waiting func()) error {
	s.mu.Lock()
	if s.writers[workspace] == key {
		s.mu.Unlock()
		return ctx.Err()
	}
	if s.waiters[key] == 0 {
		s.writerQueue[workspace] = append(s.writerQueue[workspace], key)
	}
	s.waiters[key]++
	remove := func() {
		s.waiters[key]--
		if s.waiters[key] <= 0 {
			delete(s.waiters, key)
			if s.writers[workspace] != key {
				delete(s.records, key)
			}
			q := s.writerQueue[workspace]
			for i, k := range q {
				if k == key {
					s.writerQueue[workspace] = append(q[:i], q[i+1:]...)
					break
				}
			}
		}
	}
	notified := false
	for {
		if err := ctx.Err(); err != nil {
			remove()
			s.wake()
			s.mu.Unlock()
			return err
		}
		q := s.writerQueue[workspace]
		if s.recovery[workspace] == nil && s.journalErr == nil && (s.writers[workspace] == key || (s.writers[workspace] == "" && len(q) > 0 && q[0] == key)) {
			s.writers[workspace] = key
			s.writerAcquired(workspace, key)
			if err := s.persistWorkspaceLocked(); err != nil {
				delete(s.writers, workspace)
				remove()
				s.mu.Unlock()
				return fmt.Errorf("save workspace ownership: %w", err)
			}
			remove()
			s.wake()
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		if !notified {
			waiting()
			notified = true
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
		s.mu.Lock()
	}
}
func (s *SharedCapacity) releaseWriter(workspace, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writers[workspace] == key {
		if s.writerTurns[key] > 0 {
			s.writerClosing[key] = true
			return
		}
		s.writerReleased(workspace, key)
		delete(s.writers, workspace)
		delete(s.writerClosing, key)
		_ = s.persistWorkspaceLocked()
		s.wake()
	}
}

func (s *SharedCapacity) retainWriterTurn(key string) {
	s.mu.Lock()
	s.writerTurns[key]++
	s.mu.Unlock()
}
func (s *SharedCapacity) finishWriterTurn(workspace, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writerTurns[key]--
	if s.writerTurns[key] <= 0 {
		delete(s.writerTurns, key)
		if s.writerClosing[key] && s.writers[workspace] == key {
			s.writerReleased(workspace, key)
			delete(s.writers, workspace)
			delete(s.writerClosing, key)
			_ = s.persistWorkspaceLocked()
			s.wake()
		}
	}
}

func (o *Orchestrator) ConfigureSharedCapacity(shared *SharedCapacity) error {
	workspace, err := access.CanonicalDirectory(o.room.Workspace)
	if err != nil {
		return err
	}
	if catalog, ok := o.store.(interface {
		RoomNames() (map[string]string, error)
	}); ok {
		if names, err := catalog.RoomNames(); err == nil {
			shared.mu.Lock()
			for id, name := range names {
				shared.roomNames[id] = name
			}
			shared.mu.Unlock()
		}
	}
	o.mu.Lock()
	o.sharedCapacity = shared
	o.sharedWorkspace = workspace
	o.mu.Unlock()
	return nil
}
func (o *Orchestrator) RefreshSharedCapacity() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sharedCapacity == nil {
		return
	}
	for _, provider := range chat.Agents() {
		o.sharedCapacity.setProviderLimit(provider, o.providerCapacityLocked(provider))
	}
}
func (o *Orchestrator) releaseSharedWriterLocked() {
	if o.sharedCapacity != nil && o.writerWorkflow != "" {
		o.sharedCapacity.releaseWriter(o.sharedWorkspace, o.room.ID+":"+o.writerWorkflow)
	}
}

func (o *Orchestrator) acquireSharedTurn(ctx context.Context, p chat.Participant, workflow string, emit func(agent.Event)) (func(), error) {
	o.mu.Lock()
	shared := o.sharedCapacity
	id, workspace := o.room.ID, o.sharedWorkspace
	capacity := o.providerCapacityLocked(p.Provider())
	writer := workflow != "" && o.writerWorkflow == workflow
	o.mu.Unlock()
	if shared == nil {
		return func() {}, nil
	}
	waiting := func(reason string) func() {
		return func() {
			emit(agent.Event{Type: agent.EventActivity, Agent: p, Activity: &agent.ActivityEvent{State: chat.SchedulerQueued, Action: "queued", WaitReason: reason, Transition: "manager_capacity"}})
		}
	}
	if writer {
		shared.registerWriter(id+":"+workflow, id, id, workflow, p)
		if err := shared.acquireWriter(ctx, workspace, id+":"+workflow, func() {
			a := shared.Snapshot(workspace)
			reason := "waiting for workspace writer in another room"
			if a.RecoveryRequired {
				reason = "workspace ownership requires recovery; inspect /workspace"
			} else if a.Owner != nil {
				reason = "Waiting for " + a.Owner.RoomName + "'s writable workflow"
			}
			waiting(reason)()
		}); err != nil {
			return nil, err
		}
		o.mu.Lock()
		current := o.writerWorkflow == workflow
		if !current {
			shared.releaseWriter(workspace, id+":"+workflow)
		} else {
			shared.retainWriterTurn(id + ":" + workflow)
		}
		o.mu.Unlock()
		if !current {
			return nil, fmt.Errorf("workflow ended while waiting for workspace writer")
		}
	}
	release, err := shared.acquireProvider(ctx, id, p.Provider(), capacity, waiting("waiting for provider capacity across rooms"))
	if err != nil {
		if writer {
			shared.finishWriterTurn(workspace, id+":"+workflow)
		}
		return nil, err
	}
	return func() {
		release()
		if writer {
			shared.finishWriterTurn(workspace, id+":"+workflow)
		}
	}, nil
}
