package room

import (
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

// Repair only dispositions supported by structured retained reports. Missing
// history is not evidence that business work completed or the coordinator stalled.
func migrateHandoffLedger(r *chat.CoordinationRun) bool {
	if r.LedgerVersion >= 2 {
		return false
	}
	r.LegacyResultsThrough = r.LastResultAt
	for _, e := range r.Events {
		if e.Kind == "coordinator_report" && e.Outcome == "complete" && !e.Update.HandoffOnly && e.At.After(r.CompletedThrough) {
			r.CompletedThrough = e.At
		}
	}
	if r.State == "complete" && r.CompletedThrough.IsZero() {
		// LastResultAt is safe even when the report aged out: the currently
		// completed run covers its observed results, never a future result.
		r.CompletedThrough = r.LastResultAt
	}
	for i := range r.Handoffs {
		h := &r.Handoffs[i]
		if h.Resolution == "coordinator_reported_blocked" {
			h.Resolution, h.ResolvedAt, h.WaitingOn = "", time.Time{}, "external"
		}
		if !h.Open() {
			continue
		}
		if !r.CompletedThrough.IsZero() && !h.ReadyAt.After(r.CompletedThrough) {
			h.Resolution, h.ResolvedAt = "coordinator_reported_complete", r.CompletedThrough
			continue
		}
		// Prefer the latest report for this result. A global pending report is
		// not evidence that another branch's blocker was removed.
		var latest *chat.CoordinationEvent
		for j := range r.Events {
			e := &r.Events[j]
			if e.Kind == "coordinator_report" && e.ResultID == h.ID && (latest == nil || e.At.After(latest.At)) {
				latest = e
			}
		}
		if latest != nil {
			h.AcknowledgedAt = latest.At
			if latest.Outcome == "blocked" {
				h.WaitingOn = latest.Update.WaitingOn
				if h.WaitingOn == "" {
					h.WaitingOn = "external"
				}
			}
			if latest.Outcome == "complete" || latest.Outcome == "stopped" {
				h.Resolution, h.ResolvedAt = "coordinator_reported_"+latest.Outcome, latest.At
			}
		} else if h.ReadyAt.Before(r.AcknowledgedAt) && h.AcknowledgedAt.IsZero() {
			h.NeedsReconciliation = true
		}
	}
	r.LedgerVersion = 2
	return true
}
