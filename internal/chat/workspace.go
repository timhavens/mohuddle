package chat

import "time"

// WorkspaceWriter is host-owned scheduling metadata, never another room's text.
// File reports are partial evidence, not permissions or file-level locks.
type WorkspaceWriter struct {
	RoomID        string      `json:"room_id"`
	RoomName      string      `json:"room_name,omitempty"`
	WorkflowID    string      `json:"workflow_id"`
	Participant   Participant `json:"participant,omitempty"`
	AcquiredAt    *time.Time  `json:"acquired_at,omitempty"`
	ReleasedAt    *time.Time  `json:"released_at,omitempty"`
	ReleaseReason string      `json:"release_reason,omitempty"`
	IntendedFiles []string    `json:"intended_files,omitempty"`
	ObservedFiles []string    `json:"observed_files,omitempty"`
	FilesPartial  bool        `json:"files_partial"`
}

type WorkspaceActivity struct {
	Owner            *WorkspaceWriter  `json:"owner,omitempty"`
	Waiting          []WorkspaceWriter `json:"waiting"`
	LastWriter       *WorkspaceWriter  `json:"last_writer,omitempty"`
	Revision         uint64            `json:"revision"`
	RecoveryRequired bool              `json:"recovery_required,omitempty"`
}
