package room

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func copyWriter(w *chat.WorkspaceWriter) *chat.WorkspaceWriter {
	if w == nil {
		return nil
	}
	c := *w
	c.IntendedFiles = append([]string(nil), w.IntendedFiles...)
	c.ObservedFiles = append([]string(nil), w.ObservedFiles...)
	return &c
}

func (s *SharedCapacity) registerWriter(key, room, name, workflow string, p chat.Participant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.roomNames[room]; n != "" {
		name = n
	}
	if s.records[key] == nil {
		s.records[key] = &chat.WorkspaceWriter{RoomID: room, RoomName: name, WorkflowID: workflow, Participant: p, FilesPartial: true}
	}
	s.records[key].Participant = p
}

func (s *SharedCapacity) writerAcquired(workspace, key string) {
	if r := s.records[key]; r != nil && r.AcquiredAt == nil {
		now := time.Now().UTC()
		r.AcquiredAt = &now
		s.revisions[workspace]++
	}
}
func (s *SharedCapacity) writerReleased(workspace, key string) {
	if r := s.records[key]; r != nil {
		now := time.Now().UTC()
		r.ReleasedAt = &now
		if r.ReleaseReason == "" {
			r.ReleaseReason = "workflow ended"
		}
		s.lastWriters[workspace] = copyWriter(r)
		delete(s.records, key)
		s.revisions[workspace]++
	}
}
func (s *SharedCapacity) Snapshot(workspace string) chat.WorkspaceActivity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(workspace)
}
func (s *SharedCapacity) snapshotLocked(workspace string) chat.WorkspaceActivity {
	a := chat.WorkspaceActivity{Owner: copyWriter(s.records[s.writers[workspace]]), LastWriter: copyWriter(s.lastWriters[workspace]), Revision: s.revisions[workspace], Waiting: []chat.WorkspaceWriter{}}
	if s.recovery[workspace] != nil {
		a.Owner = copyWriter(s.recovery[workspace])
		a.RecoveryRequired = true
	}
	if s.journalErr != nil {
		a.RecoveryRequired = true
	}
	for _, key := range s.writerQueue[workspace] {
		if r := copyWriter(s.records[key]); r != nil {
			a.Waiting = append(a.Waiting, *r)
		}
	}
	return a
}

// Normalize existing ancestors too: aliases of new files must not escape via a
// symlink. External paths, control characters and overlong reports are omitted.
func workspaceFile(workspace, path string) string {
	if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n\t") {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	ancestor := path
	suffix := []string{}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return ""
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return ""
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return ""
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	rel, err := filepath.Rel(workspace, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}
func mergeFiles(dst []string, workspace string, paths []string) []string {
	for i, path := range paths {
		if i >= 128 || len(dst) >= 128 {
			break
		}
		p := workspaceFile(workspace, path)
		if p == "" {
			continue
		}
		found := false
		for _, old := range dst {
			if old == p {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, p)
		}
	}
	return dst
}
func (o *Orchestrator) recordWorkspaceFiles(workflow string, intended, observed []string) {
	if workflow == "" || len(intended)+len(observed) == 0 {
		return
	}
	o.mu.Lock()
	shared, workspace, key := o.sharedCapacity, o.sharedWorkspace, o.room.ID+":"+workflow
	o.mu.Unlock()
	if shared == nil {
		return
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if r := shared.records[key]; r != nil {
		before := len(r.IntendedFiles) + len(r.ObservedFiles)
		r.IntendedFiles = mergeFiles(r.IntendedFiles, workspace, intended)
		r.ObservedFiles = mergeFiles(r.ObservedFiles, workspace, observed)
		if len(r.IntendedFiles)+len(r.ObservedFiles) != before {
			_ = shared.persistWorkspaceLocked()
		}
	}
}
func (o *Orchestrator) WorkspaceActivity() chat.WorkspaceActivity {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.workspaceActivityLocked()
}
func (o *Orchestrator) workspaceActivityLocked() chat.WorkspaceActivity {
	if o.sharedCapacity == nil {
		return chat.WorkspaceActivity{Waiting: []chat.WorkspaceWriter{}}
	}
	return o.sharedCapacity.Snapshot(o.sharedWorkspace)
}
func (o *Orchestrator) workspaceWriterGuidance(workflow string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sharedCapacity == nil || workflow == "" || o.writerWorkflow != workflow {
		return ""
	}
	a := o.sharedCapacity.Snapshot(o.sharedWorkspace)
	text := "\n\nMoHuddle shared-checkout coordination: the host owns the writable workflow reservation. Before editing, reread the current contents of every target file and reconcile any older draft with them; another room may have changed this checkout while you prepared or waited. Preserve unrelated changes. Do not negotiate locks with ChatGPT. In your existing terminal mohuddle control marker you may report intended_files as an array of workspace-relative paths for the next step; these are partial intentions, not permissions."
	if a.LastWriter != nil {
		text += fmt.Sprintf(" Previous writable workflow: %s (%s), %s. It may have modified files since earlier preparation; its file reports are incomplete. Reread before applying changes.", a.LastWriter.RoomName, a.LastWriter.RoomID, a.LastWriter.WorkflowID)
	}
	return text
}

func FormatWorkspaceActivity(a chat.WorkspaceActivity) string {
	lines := []string{"Shared checkout: one writable workflow at a time; file reports are partial."}
	if a.RecoveryRequired {
		lines = append(lines, "Recovery required: verify interrupted native workers have stopped, then confirm locally with /workspace recover stopped. Writers remain blocked; read-only work may continue.")
	}
	ownerLabel := "Writing"
	if a.RecoveryRequired {
		ownerLabel = "Interrupted or uncertain writer"
	}
	for _, group := range []struct {
		label   string
		entries []chat.WorkspaceWriter
	}{{ownerLabel, writerList(a.Owner)}, {"Waiting", a.Waiting}, {"Last writer", writerList(a.LastWriter)}} {
		for _, r := range group.entries {
			lines = append(lines, fmt.Sprintf("%s: %s · @%s · %s", group.label, r.RoomName, r.Participant, r.WorkflowID))
			intended, observed := "unknown", "unknown"
			if len(r.IntendedFiles) > 0 {
				intended = strings.Join(r.IntendedFiles, ", ")
			}
			if len(r.ObservedFiles) > 0 {
				observed = strings.Join(r.ObservedFiles, ", ")
			}
			lines = append(lines, "  Reported intended files: "+intended+"; provider-observed files: "+observed)
			if r.ReleaseReason != "" {
				lines = append(lines, "  "+r.ReleaseReason)
			}
		}
	}
	return strings.Join(lines, "\n")
}
func writerList(w *chat.WorkspaceWriter) []chat.WorkspaceWriter {
	if w == nil {
		return nil
	}
	return []chat.WorkspaceWriter{*w}
}
