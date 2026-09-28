package api

import (
	"fmt"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

type followUpController interface {
	ObserveFollowUps(chat.FollowUpUpdate, time.Time) (chat.FollowUpView, error)
}

type FollowUpRequest struct {
	ParticipationID string `json:"participation_id"`
	chat.FollowUpUpdate
}

func (s *Service) followUpsLocked(request Request) HandleResult {
	u, err := decodeChatGPTPayload[FollowUpRequest](request)
	if err != nil || !validIdentifier(u.EventID) {
		return failed(request, "invalid_request", "invalid follow-up request")
	}
	if !s.validParticipationLocked(u.ParticipationID) {
		return failed(request, "not_joined", "join before controlling follow-ups")
	}
	if u.ResultID != "" || u.RunID != "" {
		return failed(request, "invalid_request", "use mohuddle_notification for a retained handoff")
	}
	if u.Through > s.chatgpt.delivered {
		return failed(request, "invalid_cursor", "read the update before notifying")
	}
	if u.Automatic && u.Stage == "notification_attempted" && len(u.UpdateKey) != 64 {
		return failed(request, "invalid_request", "use notification_key from the latest read")
	}
	if (u.Stage == "pause" || u.Stage == "resume" || u.Stage == "renew" || u.Automatic && u.Stage == "notification_attempted") && u.Revision == 0 {
		return failed(request, "invalid_request", "current follow-up revision required")
	}
	s.updateChatGPTStateLocked()
	c, ok := s.controller.(followUpController)
	if !ok {
		return failed(request, "unsupported", "persistent follow-ups unavailable")
	}
	v, err := c.ObserveFollowUps(u.FollowUpUpdate, time.Now().UTC())
	if err != nil {
		return failed(request, "invalid_request", err.Error())
	}
	return succeeded(request, v)
}

// ControlFollowUps is a trusted local control, never a room slash-command post.
func (s *Service) ControlFollowUps(action string) (chat.FollowUpView, error) {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	s.updateChatGPTStateLocked()
	state, _ := s.controller.Snapshot()
	v := state.FollowUps.View(time.Now().UTC(), state.ChatGPT, state.Coordination)
	if action == "status" {
		return v, nil
	}
	if action == "on" {
		action = "resume"
	}
	if action == "off" {
		action = "pause"
	}
	if action != "pause" && action != "resume" && action != "renew" {
		return v, fmt.Errorf("use followups on|off|renew|status")
	}
	c, ok := s.controller.(followUpController)
	if !ok {
		return v, fmt.Errorf("persistent follow-ups unavailable")
	}
	id, err := NewID()
	if err != nil {
		return v, err
	}
	return c.ObserveFollowUps(chat.FollowUpUpdate{Stage: action, EventID: id, Revision: v.Revision}, time.Now().UTC())
}
