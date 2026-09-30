package room

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

// ConfigureWorkspaceJournal is called after the manager has acquired its local
// gateway, before dispatch. An unfinished owner survives as a recovery hold:
// process death alone does not prove that every native child stopped writing.
func (s *SharedCapacity) ConfigureWorkspaceJournal(data []byte, save func([]byte) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := map[string]chat.WorkspaceActivity{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("read workspace ownership journal: %w", err)
		}
	}
	for workspace, a := range saved {
		s.lastWriters[workspace] = copyWriter(a.LastWriter)
		s.revisions[workspace] = a.Revision
		if a.Owner != nil {
			s.recovery[workspace] = copyWriter(a.Owner)
		}
	}
	s.saveJournal = save
	return nil
}
func (s *SharedCapacity) persistWorkspaceLocked() error {
	if s.saveJournal == nil {
		return nil
	}
	saved := map[string]chat.WorkspaceActivity{}
	for workspace := range s.revisions {
		saved[workspace] = s.snapshotLocked(workspace)
	}
	for workspace := range s.recovery {
		saved[workspace] = s.snapshotLocked(workspace)
	}
	data, err := json.Marshal(saved)
	if err == nil {
		err = s.saveJournal(data)
	}
	s.journalErr = err
	return err
}

// ConfirmWorkspaceStopped is a local human recovery action, never a model tool.
// The host must verify orphaned native workers have stopped before invoking it.
func (o *Orchestrator) ConfirmWorkspaceStopped() error {
	o.mu.Lock()
	s, workspace := o.sharedCapacity, o.sharedWorkspace
	o.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writers[workspace] != "" {
		return fmt.Errorf("a current writer is still active; wait for it to stop")
	}
	old := s.recovery[workspace]
	if old != nil {
		r := copyWriter(old)
		now := time.Now().UTC()
		r.ReleasedAt = &now
		r.ReleaseReason = "host confirmed interrupted native workers stopped"
		s.lastWriters[workspace] = r
		delete(s.recovery, workspace)
		s.revisions[workspace]++
	}
	if err := s.persistWorkspaceLocked(); err != nil {
		if old != nil {
			s.recovery[workspace] = old
		}
		return err
	}
	s.wake()
	return nil
}
