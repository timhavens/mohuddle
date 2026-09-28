package room

import (
	"fmt"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

// ObserveFollowUps serializes controls, availability and every notification claim
// across panels. Persist before returning permission to send a website message.
func (o *Orchestrator) ObserveFollowUps(u chat.FollowUpUpdate, now time.Time) (chat.FollowUpView, error) {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	previous, previousRun := o.room.FollowUps.Clone(), o.room.Coordination.Clone()
	if err := o.applyFollowUpsLocked(u, now); err != nil {
		o.room.FollowUps, o.room.Coordination = previous, previousRun
		return chat.FollowUpView{}, err
	}
	if err := o.store.SaveRoom(cloneRoom(o.room)); err != nil {
		o.room.FollowUps, o.room.Coordination = previous, previousRun
		return chat.FollowUpView{}, err
	}
	return o.room.FollowUps.View(now, o.room.ChatGPT, o.room.Coordination), nil
}

func (o *Orchestrator) ensureFollowUpsLocked() *chat.FollowUps {
	if o.room.FollowUps == nil {
		f := &chat.FollowUps{Enabled: true, Revision: 1}
		// Import observable legacy consumption; a new panel must not replenish it.
		if r := o.room.Coordination; r != nil {
			f.Panels = append([]chat.CoordinationPanel(nil), r.Panels...)
			f.LastNotificationAt = r.LastNotificationAt
			for _, h := range r.Handoffs {
				for _, a := range h.Attempts {
					if a.Automatic {
						f.Attempts++
						if f.StartedAt.IsZero() || a.At.Before(f.StartedAt) {
							f.StartedAt = a.At
						}
					}
				}
			}
		}
		o.room.FollowUps = f
	}
	return o.room.FollowUps
}

func (o *Orchestrator) applyFollowUpsLocked(u chat.FollowUpUpdate, now time.Time) error {
	if u.EventID == "" || len(u.EventID) > 128 || len(u.PanelID) > 128 || len(u.ResultID) > 128 || len(u.RunID) > 128 || len(u.UpdateKey) > 64 {
		return fmt.Errorf("invalid follow-up observation")
	}
	f := o.ensureFollowUpsLocked()
	if u.Stage == "pause" || u.Stage == "resume" || u.Stage == "renew" {
		if f.LastControlID == u.EventID {
			if f.LastControl != u.Stage || f.LastControlRevision != u.Revision {
				return fmt.Errorf("control ID reused with different content")
			}
			return nil
		}
		if u.Revision != f.Revision {
			return fmt.Errorf("follow-up setting changed; refresh before controlling it")
		}
		f.Enabled = u.Stage != "pause"
		if u.Stage == "renew" {
			f.StartedAt = now
			f.Attempts = 0
		}
		f.LastControlID, f.LastControl, f.LastControlRevision = u.EventID, u.Stage, u.Revision
		f.Revision++
		return nil
	}
	if u.Stage == "panel_status" {
		switch u.Delivery {
		case "enabled", "manual", "hidden", "unsupported", "disconnected", "budget", "closed", "unknown":
		default:
			return fmt.Errorf("invalid panel delivery state")
		}
		if u.PanelID == "" {
			return fmt.Errorf("panel_id required")
		}
		// Availability never changes the desired setting. Stale panels cannot unpause.
		if f.StartedAt.IsZero() && u.Delivery == "enabled" && f.Enabled {
			f.StartedAt = now
		}
		for i := range f.Panels {
			if f.Panels[i].ID == u.PanelID {
				f.Panels[i].Mode, f.Panels[i].SeenAt = u.Delivery, now
				return nil
			}
		}
		if len(f.Panels) >= 8 {
			f.Panels = f.Panels[1:]
		}
		f.Panels = append(f.Panels, chat.CoordinationPanel{ID: u.PanelID, Mode: u.Delivery, SeenAt: now, StartedAt: now})
		return nil
	}
	if u.Stage != "notification_attempted" && u.Stage != "host_accepted" && u.Stage != "host_rejected" && u.Stage != "host_unknown" {
		return fmt.Errorf("invalid follow-up stage")
	}
	var claim *chat.FollowUpClaim
	for i := range f.Claims {
		if f.Claims[i].ID == u.EventID {
			claim = &f.Claims[i]
			break
		}
	}
	if claim != nil {
		if claim.UpdateKey != u.UpdateKey || claim.ResultID != u.ResultID || claim.Through != u.Through || claim.RunID != u.RunID || claim.PanelID != u.PanelID || claim.Automatic != u.Automatic {
			return fmt.Errorf("notification claim ID reused with different content")
		}
		if u.Stage != "notification_attempted" {
			if claim.Outcome != "host_unknown" && claim.Outcome != u.Stage {
				return fmt.Errorf("notification outcome already recorded")
			}
			claim.Outcome = u.Stage
		}
		return nil
	}
	if u.Stage != "notification_attempted" {
		return fmt.Errorf("notification claim unavailable; outcome unknown")
	}
	state := o.room.ChatGPT
	if state == nil || !state.Enabled || !state.Connected || state.Paused || f.HostPaused || !state.ExpiresAt.IsZero() && !now.Before(state.ExpiresAt) || o.room.Coordination != nil && o.room.Coordination.State == "stopped" {
		return fmt.Errorf("notification delivery is stopped or participation is unavailable")
	}
	if u.Automatic {
		if u.Revision != 0 && u.Revision != f.Revision {
			return fmt.Errorf("follow-up setting changed; refresh")
		}
		v := f.View(now, o.room.ChatGPT, o.room.Coordination)
		if v.Reason != "" && v.Reason != "host_rejected" && v.Reason != "host_unknown" {
			return fmt.Errorf("automatic follow-ups unavailable: %s", v.Reason)
		}
		available := false
		for _, p := range f.Panels {
			if p.ID == u.PanelID && p.Mode == "enabled" && now.Sub(p.SeenAt) <= 45*time.Second {
				available = true
			}
		}
		if !available {
			return fmt.Errorf("register this panel's availability first")
		}
	}
	if now.Sub(f.LastNotificationAt) < 20*time.Second {
		return fmt.Errorf("another panel recently claimed a notification")
	}
	if u.ResultID == "" && u.Automatic {
		if u.UpdateKey != "" {
			if u.UpdateKey != chat.NotificationKey(o.room, o.messages) {
				return fmt.Errorf("room update changed; read again before notifying")
			}
			if u.UpdateKey == f.NotifiedUpdateKey {
				return fmt.Errorf("room update already claimed")
			}
		} else if u.Through == 0 || u.Through <= f.NotifiedThrough {
			return fmt.Errorf("room update already claimed")
		}
	}
	if f.StartedAt.IsZero() && u.Automatic {
		f.StartedAt = now
	}
	if u.Automatic {
		f.Attempts++
	}
	f.LastNotificationAt = now
	if u.ResultID == "" {
		f.NotifiedThrough = max(f.NotifiedThrough, u.Through)
		if u.UpdateKey != "" {
			f.NotifiedUpdateKey = u.UpdateKey
		}
	}
	f.Claims = append(f.Claims, chat.FollowUpClaim{NotificationAttempt: chat.NotificationAttempt{ID: u.EventID, PanelID: u.PanelID, Automatic: u.Automatic, At: now, Outcome: "host_unknown"}, UpdateKey: u.UpdateKey, ResultID: u.ResultID, RunID: u.RunID, Through: u.Through})
	if len(f.Claims) > 256 {
		f.Claims = append([]chat.FollowUpClaim(nil), f.Claims[len(f.Claims)-256:]...)
	}
	return nil
}
