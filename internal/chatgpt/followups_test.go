//go:build !windows

package chatgpt

import (
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
)

// Website messages and the coordinator are simulated; the MCP transport, API,
// persistence, participants and assignment scheduler all execute normally.
func TestMCPAutomaticHandoffToNextAssignment(t *testing.T) {
	b, service, _, _ := testBridge(t)
	run, err := service.ControlCoordination("start")
	if err != nil {
		t.Fatal(err)
	}
	client := mcpClient(t, b)
	v := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if !v.FollowUps.Enabled {
		t.Fatal("joining requires a manual Enable click")
	}
	f := callMCP[chat.FollowUpView](t, client, "mohuddle_followups", api.FollowUpRequest{ParticipationID: v.ParticipationID, FollowUpUpdate: chat.FollowUpUpdate{Stage: "panel_status", EventID: "panel", PanelID: "live", Delivery: "enabled"}})
	if f.Status != "On and connected" {
		t.Fatal(f)
	}
	w := callMCP[WorkOutput](t, client, "mohuddle_request_work", api.ChatGPTWorkRequest{ParticipationID: v.ParticipationID, OperationID: "writer", Target: chat.Codex, Text: "Prepare the authorized draft", Coordination: &chat.CoordinationDispatch{RunID: run.ID}})
	var h chat.Handoff
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: v.ParticipationID, After: v.NextAfter})
		for _, candidate := range v.Coordination.Handoffs {
			if candidate.ID == "work:"+w.WorkflowID {
				h = candidate
			}
		}
		if h.ResultSequence != 0 && h.NotificationDue {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !h.NotificationDue {
		t.Fatal("writer did not create actionable handoff", h)
	}
	n := api.NotificationRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "notify", ResultID: h.ID, PanelID: "live", Automatic: true, Stage: "notification_attempted"}
	claim := callMCP[chat.CoordinationView](t, client, "mohuddle_notification", n)
	if claim.FollowUps.Remaining != 31 {
		t.Fatal("handoff did not consume shared allowance")
	}
	n.Stage = "host_accepted"
	accepted := callMCP[chat.CoordinationView](t, client, "mohuddle_notification", n)
	if !accepted.Handoffs[0].Open() || !accepted.Handoffs[0].AcknowledgedAt.IsZero() {
		t.Fatal("delivery falsely acknowledged result")
	}
	page := callMCP[api.ChatGPTMessagePage](t, client, "mohuddle_read_message", api.ChatGPTMessageRequest{ParticipationID: v.ParticipationID, Sequence: h.ResultSequence})
	if !page.Complete || page.Text == "" {
		t.Fatal("actual draft unavailable")
	}
	callMCP[chat.CoordinationView](t, client, "mohuddle_coordinator_report", api.CoordinatorReportRequest{ParticipationID: v.ParticipationID, RunID: run.ID, EventID: "ack", ResultID: h.ID, State: "pending", Detail: "Review the actual draft", NextAction: "Independent review", WaitingOn: "coordinator"})
	next := api.ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "review", Text: "Review this draft: " + page.Text, RequestReplies: []chat.Participant{chat.Claude}, Coordination: &chat.CoordinationDispatch{RunID: run.ID, HandoffID: h.ID}}
	receipt := callMCP[PublishOutput](t, client, "mohuddle_publish", next)
	duplicate := callMCP[PublishOutput](t, client, "mohuddle_publish", next)
	v = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: v.ParticipationID})
	if !duplicate.Duplicate || receipt.Sequence != duplicate.Sequence || v.Coordination.Handoffs[0].AssignmentSequence != receipt.Sequence || v.Coordination.Handoffs[0].Open() {
		t.Fatal("linked next step lost or duplicated")
	}
}
