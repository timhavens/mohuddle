package chat

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

// FollowUps belongs to the room, not a panel, run, participation, or credential.
// Nil in an older room means enabled, with an allowance beginning on first use.
type FollowUps struct {
	HostPaused          bool                `json:"host_paused,omitempty"`
	Enabled             bool                `json:"enabled"`
	Revision            uint64              `json:"revision"`
	StartedAt           time.Time           `json:"started_at,omitzero"`
	Attempts            int                 `json:"attempts"`
	LastNotificationAt  time.Time           `json:"last_notification_at,omitzero"`
	NotifiedThrough     uint64              `json:"notified_through"`
	NotifiedUpdateKey   string              `json:"notified_update_key,omitempty"`
	Panels              []CoordinationPanel `json:"panels,omitempty"`
	Claims              []FollowUpClaim     `json:"claims,omitempty"`
	LastControlID       string              `json:"last_control_id,omitempty"`
	LastControl         string              `json:"last_control,omitempty"`
	LastControlRevision uint64              `json:"last_control_revision,omitempty"`
}

type FollowUpClaim struct {
	NotificationAttempt
	UpdateKey string `json:"update_key,omitempty"`
	ResultID  string `json:"result_id,omitempty"`
	Through   uint64 `json:"through,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

type FollowUpUpdate struct {
	UpdateKey string `json:"update_key,omitempty" jsonschema:"notification_key from the latest room view, for a general automatic update. Distinguishes result changes without new message text."`
	Stage     string `json:"stage" jsonschema:"panel_status, pause, resume, renew, notification_attempted, host_accepted, host_rejected, or host_unknown. Controls require a user action; renew explicitly resets the shared allowance."`
	EventID   string `json:"event_id"`
	PanelID   string `json:"panel_id,omitempty"`
	Delivery  string `json:"delivery,omitempty"`
	Revision  uint64 `json:"revision,omitempty" jsonschema:"Current follow_ups.revision; required for pause/resume/renew and new automatic claims."`
	Automatic bool   `json:"automatic,omitempty"`
	Through   uint64 `json:"through,omitempty" jsonschema:"Delivered message cursor for a general update; omitted for a handoff."`
	RunID     string `json:"run_id,omitempty"`
	ResultID  string `json:"result_id,omitempty"`
}

// NotificationKey identifies public updates, including terminal operations
// without a new answer message. It contains no prompt or provider-private data.
func NotificationKey(room Room, messages []Message) string {
	var parts []string
	var sequence uint64
	for _, m := range messages {
		if m.Kind == MessageText && (m.Author == User || m.Author.ValidAgent()) {
			sequence = max(sequence, m.Sequence)
		}
	}
	parts = append(parts, fmt.Sprint(sequence))
	for _, w := range room.Workflows {
		if w.State != WorkflowQueued && w.State != WorkflowActive && w.State != WorkflowWaiting {
			parts = append(parts, fmt.Sprintf("work:%s:%s:%s", w.ID, w.State, w.UpdatedAt.Format(time.RFC3339Nano)))
		}
	}
	for _, j := range room.Conversations {
		if j.State.Terminal() && j.CompletedAt != nil {
			parts = append(parts, fmt.Sprintf("reply:%s:%s:%s", j.ID, j.State, j.CompletedAt.Format(time.RFC3339Nano)))
		}
	}
	sort.Strings(parts)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\n"))))
}

type FollowUpView struct {
	Enabled            bool      `json:"enabled"`
	Revision           uint64    `json:"revision"`
	StartedAt          time.Time `json:"started_at,omitzero"`
	Remaining          int       `json:"remaining"`
	SecondsRemaining   int64     `json:"seconds_remaining"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	NotifiedThrough    uint64    `json:"notified_through"`
	LastNotificationAt time.Time `json:"last_notification_at,omitzero"`
	LastOutcome        string    `json:"last_outcome,omitempty"`
}

func (f *FollowUps) Clone() *FollowUps {
	if f == nil {
		return nil
	}
	c := *f
	c.Panels = append([]CoordinationPanel(nil), f.Panels...)
	c.Claims = append([]FollowUpClaim(nil), f.Claims...)
	return &c
}

func (f *FollowUps) View(now time.Time, state *ChatGPTState, run *CoordinationRun) FollowUpView {
	if f == nil {
		f = &FollowUps{Enabled: true, Revision: 1}
	}
	limits := DefaultChatGPTLimits()
	if state != nil && state.Limits.FollowUps > 0 {
		limits = state.Limits
	}
	v := FollowUpView{Enabled: f.Enabled, Revision: f.Revision, StartedAt: f.StartedAt, Remaining: max(0, limits.FollowUps-f.Attempts), SecondsRemaining: int64(limits.FollowUpSeconds), NotifiedThrough: f.NotifiedThrough, LastNotificationAt: f.LastNotificationAt, Status: "Panel unavailable", Reason: "panel_unavailable"}
	if !f.StartedAt.IsZero() {
		v.SecondsRemaining = max(0, int64(f.StartedAt.Add(time.Duration(limits.FollowUpSeconds)*time.Second).Sub(now)/time.Second))
	}
	if len(f.Claims) > 0 {
		v.LastOutcome = f.Claims[len(f.Claims)-1].Outcome
	}
	switch {
	case !f.Enabled:
		v.Status, v.Reason = "Paused", "manual"
	case run != nil && run.State == "stopped":
		v.Status, v.Reason = "Paused", "run_stopped"
	case state == nil || !state.Enabled || !state.Connected || !state.ExpiresAt.IsZero() && !now.Before(state.ExpiresAt):
		v.Reason = "disconnected"
	case f.HostPaused || state.Paused:
		v.Status, v.Reason = "Paused", "host_paused"
	case state.PauseReason != "":
		v.Status, v.Reason = "Paused", state.PauseReason
	case v.Remaining == 0:
		v.Status, v.Reason = "Limit reached", "follow_up_limit"
	case v.SecondsRemaining == 0:
		v.Status, v.Reason = "Limit reached", "time_limit"
	default:
		for _, p := range f.Panels {
			if now.Sub(p.SeenAt) > 45*time.Second {
				continue
			}
			v.Reason = p.Mode
			if p.Mode == "enabled" {
				v.Status, v.Reason = "On and connected", ""
				break
			}
		}
		if v.LastOutcome == "host_rejected" && v.Reason == "" {
			v.Status, v.Reason = "Delivery rejected", "host_rejected"
		}
		if v.LastOutcome == "host_unknown" && v.Reason == "" {
			v.Status, v.Reason = "Delivery outcome unknown", "host_unknown"
		}
	}
	return v
}
