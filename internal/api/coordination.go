package api

import (
	"fmt"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

type coordinationController interface {
	ControlCoordination(string, time.Time) (chat.CoordinationView, error)
	CoordinationStatus(time.Time) (chat.CoordinationView, error)
	ReportCoordination(string, string, string, string, string, string, time.Time, ...chat.CoordinatorUpdate) (chat.CoordinationView, error)
}

func (s *Service) ControlCoordination(action string) (*chat.CoordinationView, error) {
	c, ok := s.controller.(coordinationController)
	if !ok {
		return nil, fmt.Errorf("coordination monitoring unavailable")
	}
	v, err := c.ControlCoordination(action, time.Now().UTC())
	return &v, err
}

func (s *Service) CoordinationStatus(now time.Time) (*chat.CoordinationView, error) {
	c, ok := s.controller.(coordinationController)
	if !ok {
		return nil, nil
	}
	v, err := c.CoordinationStatus(now)
	if v.CoordinationRun == nil {
		return nil, err
	}
	return &v, err
}

type CoordinatorReportRequest struct {
	HandoffOnly bool                        `json:"handoff_only,omitempty" jsonschema:"Apply this status only to result_id; a blocked or completed branch does not stop independent work."`
	Objective   *chat.CoordinationObjective `json:"objective,omitempty" jsonschema:"Coordinator record of the user objective, authorized scope and completion criteria; not a new authority grant."`
	NextAction  string                      `json:"next_action,omitempty"`
	Owner       string                      `json:"owner,omitempty"`
	WaitingFor  string                      `json:"waiting_for,omitempty" jsonschema:"An existing active workflow or reply ID that genuinely blocks this handoff."`

	ParticipationID string `json:"participation_id"`
	RunID           string `json:"run_id" jsonschema:"Current locally enabled monitored run. Cannot start or resume a run."`
	EventID         string `json:"event_id" jsonschema:"Unique event identifier; reuse only for an identical retry."`
	ResultID        string `json:"result_id,omitempty" jsonschema:"Retained result_available result_id explicitly read and acknowledged. Omit for status only."`
	State           string `json:"state" jsonschema:"pending, blocked, complete, or stopped. Reporting complete is not verified task completion."`
	Detail          string `json:"detail" jsonschema:"Concise next action or blocker, maximum 1024 UTF-8 bytes. No private discussion."`
}

type NotificationRequest struct {
	Automatic bool   `json:"automatic,omitempty"`
	PanelID   string `json:"panel_id,omitempty"`
	Delivery  string `json:"delivery,omitempty"`

	ParticipationID string `json:"participation_id"`
	RunID           string `json:"run_id"`
	EventID         string `json:"event_id"`
	ResultID        string `json:"result_id,omitempty" jsonschema:"Required for notification claims and outcomes; omit for panel_status."`
	Stage           string `json:"stage" jsonschema:"notification_attempted (atomic claim), host_accepted, host_rejected, host_unknown, or panel_status. Panel observation only; never coordinator acknowledgement."`
}

func (s *Service) coordinationReportLocked(request Request) HandleResult {
	var v CoordinatorReportRequest
	kind := "coordinator_report"
	var err error
	update := chat.CoordinatorUpdate{}
	if request.Type == "chatgpt.notification" {
		var n NotificationRequest
		n, err = decodeChatGPTPayload[NotificationRequest](request)
		v = CoordinatorReportRequest{ParticipationID: n.ParticipationID, RunID: n.RunID, EventID: n.EventID, ResultID: n.ResultID}
		kind = n.Stage
		update = chat.CoordinatorUpdate{Automatic: n.Automatic, PanelID: n.PanelID, Delivery: n.Delivery}
		if kind != "notification_attempted" && kind != "host_accepted" && kind != "host_rejected" && kind != "host_unknown" && kind != "panel_status" {
			return failed(request, "invalid_request", "invalid notification stage")
		}
	} else {
		v, err = decodeChatGPTPayload[CoordinatorReportRequest](request)
		update = chat.CoordinatorUpdate{HandoffOnly: v.HandoffOnly, Objective: v.Objective, NextAction: v.NextAction, Owner: v.Owner, WaitingFor: v.WaitingFor}
	}
	if err != nil || !validIdentifier(v.EventID) {
		return failed(request, "invalid_request", "invalid report")
	}
	if !s.validParticipationLocked(v.ParticipationID) {
		return failed(request, "not_joined", "join this room before reporting")
	}
	c, ok := s.controller.(coordinationController)
	if !ok {
		return failed(request, "unsupported", "monitoring unavailable")
	}
	// Explicit acknowledgement must refer to a result this participation has
	// actually been offered; a panel poll never calls this report endpoint.
	if v.ResultID != "" {
		view, e := c.CoordinationStatus(time.Now().UTC())
		if e != nil {
			return failed(request, "persistence_error", e.Error())
		}
		found := false
		if view.CoordinationRun != nil {
			for _, h := range view.Handoffs {
				if h.ID == v.ResultID && h.SourceSequence <= s.chatgpt.delivered && (kind != "coordinator_report" || h.ResultSequence <= s.chatgpt.delivered) {
					found = true
				}
			}
			for _, event := range view.Events {
				if len(view.Handoffs) == 0 && event.ResultID == v.ResultID && event.Kind == "result_available" && event.SourceSequence <= s.chatgpt.delivered {
					found = true
				}
			}
		}
		if !found {
			return failed(request, "invalid_request", "read the result's source before acknowledging it")
		}
	}
	if update.Automatic && kind == "notification_attempted" {
		s.updateChatGPTStateLocked()
		state, _ := s.controller.Snapshot()
		if state.ChatGPT == nil || state.ChatGPT.Paused || state.ChatGPT.PauseReason != "" {
			return failed(request, "paused", "automatic follow-ups are paused")
		}
	}
	view, err := c.ReportCoordination(v.RunID, v.EventID, kind, v.ResultID, v.State, v.Detail, time.Now().UTC(), update)
	if err != nil {
		return failed(request, "invalid_report", err.Error())
	}
	return succeeded(request, view)
}
