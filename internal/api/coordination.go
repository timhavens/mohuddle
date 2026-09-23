package api

import (
	"fmt"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

type coordinationController interface {
	ControlCoordination(string, time.Time) (chat.CoordinationView, error)
	CoordinationStatus(time.Time) (chat.CoordinationView, error)
	ReportCoordination(string, string, string, string, string, string, time.Time) (chat.CoordinationView, error)
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
	ParticipationID string `json:"participation_id"`
	RunID           string `json:"run_id" jsonschema:"Current locally enabled monitored run. Cannot start or resume a run."`
	EventID         string `json:"event_id" jsonschema:"Unique event identifier; reuse only for an identical retry."`
	ResultID        string `json:"result_id,omitempty" jsonschema:"Retained result_available result_id explicitly read and acknowledged. Omit for status only."`
	State           string `json:"state" jsonschema:"pending, blocked, complete, or stopped. Reporting complete is not verified task completion."`
	Detail          string `json:"detail" jsonschema:"Concise next action or blocker, maximum 1024 UTF-8 bytes. No private discussion."`
}

type NotificationRequest struct {
	ParticipationID string `json:"participation_id"`
	RunID           string `json:"run_id"`
	EventID         string `json:"event_id"`
	ResultID        string `json:"result_id"`
	Stage           string `json:"stage" jsonschema:"notification_attempted, host_accepted, host_rejected, or host_unknown. Panel observation only; never coordinator acknowledgement."`
}

func (s *Service) coordinationReportLocked(request Request) HandleResult {
	var v CoordinatorReportRequest
	kind := "coordinator_report"
	var err error
	if request.Type == "chatgpt.notification" {
		var n NotificationRequest
		n, err = decodeChatGPTPayload[NotificationRequest](request)
		v = CoordinatorReportRequest{ParticipationID: n.ParticipationID, RunID: n.RunID, EventID: n.EventID, ResultID: n.ResultID}
		kind = n.Stage
		if kind != "notification_attempted" && kind != "host_accepted" && kind != "host_rejected" && kind != "host_unknown" {
			return failed(request, "invalid_request", "invalid notification stage")
		}
	} else {
		v, err = decodeChatGPTPayload[CoordinatorReportRequest](request)
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
			for _, event := range view.Events {
				if event.ResultID == v.ResultID && event.Kind == "result_available" && event.SourceSequence <= s.chatgpt.delivered {
					found = true
				}
			}
		}
		if !found {
			return failed(request, "invalid_request", "read the result's source before acknowledging it")
		}
	}
	view, err := c.ReportCoordination(v.RunID, v.EventID, kind, v.ResultID, v.State, v.Detail, time.Now().UTC())
	if err != nil {
		return failed(request, "invalid_report", err.Error())
	}
	return succeeded(request, view)
}
