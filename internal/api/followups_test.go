//go:build !windows

package api

import (
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	roomguidance "github.com/timhavens/mohuddle/internal/chatgpt/skills/mohuddle-room"
)

func TestFollowUpsAPIWorksWithoutMonitorAndPreservesRejoin(t *testing.T) {
	s, o, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	opened := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: v.ParticipationID})
	if !opened.OK {
		t.Fatal(opened.Error)
	}
	token := opened.Result.(ChatGPTView).PanelToken
	if !v.FollowUps.Enabled || v.ToolContractVersion != roomguidance.ToolContractVersion || !strings.Contains(v.ClientCompatibility, "unknown") {
		t.Fatal(v)
	}
	r := chatGPTCall(t, s, session, "chatgpt.followups", FollowUpRequest{ParticipationID: v.ParticipationID, PanelToken: token, FollowUpUpdate: chat.FollowUpUpdate{Stage: "pause", EventID: "pause", Revision: v.FollowUps.Revision}})
	if !r.OK || r.Result.(chat.FollowUpView).Enabled {
		t.Fatal(r)
	}
	r = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: v.ParticipationID, ClientContractVersion: roomguidance.ToolContractVersion})
	if !r.OK || !strings.Contains(r.Result.(ChatGPTView).ClientCompatibility, "client reports") {
		t.Fatal(r)
	}
	v = joinChatGPT(t, s, session)
	if v.FollowUps.Enabled {
		t.Fatal("rejoin cleared pause")
	}
	r = chatGPTCall(t, s, session, "chatgpt.followups", FollowUpRequest{ParticipationID: v.ParticipationID, PanelToken: token, FollowUpUpdate: chat.FollowUpUpdate{Stage: "resume", EventID: "stale", Revision: 1}})
	if r.OK {
		t.Fatal("stale control accepted")
	}
	u := FollowUpRequest{ParticipationID: v.ParticipationID, PanelToken: token, FollowUpUpdate: chat.FollowUpUpdate{Stage: "resume", EventID: "resume", Revision: v.FollowUps.Revision}}
	r = chatGPTCall(t, s, session, "chatgpt.followups", u)
	if !r.OK {
		t.Fatal(r.Error)
	}
	f := r.Result.(chat.FollowUpView)
	u.FollowUpUpdate = chat.FollowUpUpdate{Stage: "panel_status", EventID: "panel", PanelID: "one", Delivery: "enabled"}
	if r = chatGPTCall(t, s, session, "chatgpt.followups", u); !r.OK {
		t.Fatal(r.Error)
	}
	if err := o.Post("Please continue the authorized task"); err != nil {
		t.Fatal(err)
	}
	// Notification claims require the exact public update that was read. Wait
	// for the fixture's asynchronous work to settle before testing that claim;
	// a result arriving between read and claim is correctly rejected as stale.
	deadline := time.Now().Add(3 * time.Second)
	for o.HasActiveWork() {
		if time.Now().After(deadline) {
			t.Fatal("fixture work did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	u.FollowUpUpdate = chat.FollowUpUpdate{Stage: "notification_attempted", EventID: "attempt", PanelID: "one", Automatic: true, Through: v.NextAfter + 1, Revision: f.Revision}
	if r = chatGPTCall(t, s, session, "chatgpt.followups", u); r.OK {
		t.Fatal("undelivered cursor claimed")
	}
	r = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: v.ParticipationID})
	if !r.OK {
		t.Fatal(r.Error)
	}
	v = r.Result.(ChatGPTView)
	u.Through = v.NextAfter
	u.UpdateKey = v.NotificationKey
	if r = chatGPTCall(t, s, session, "chatgpt.followups", u); !r.OK || r.Result.(chat.FollowUpView).Remaining != 31 {
		t.Fatalf("notification claim: response=%+v error=%+v", r, r.Error)
	}
	u.Stage = "host_accepted"
	if r = chatGPTCall(t, s, session, "chatgpt.followups", u); !r.OK || r.Result.(chat.FollowUpView).LastOutcome != "host_accepted" {
		t.Fatal(r)
	}
	if v.Coordination != nil {
		t.Fatal("follow-ups must not require or start a monitored run")
	}
}

func TestLegacyJoinWidgetsCannotControlPanelDelivery(t *testing.T) {
	s, _, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	for _, kind := range []string{"chatgpt.followups", "chatgpt.notification"} {
		payload := map[string]any{"participation_id": v.ParticipationID, "event_id": "legacy", "stage": "panel_status", "panel_id": "legacy", "delivery": "enabled"}
		r := chatGPTCall(t, s, session, kind, payload)
		if r.OK || r.Error.Code != "incompatible_client" {
			t.Fatal("unattached widget registered delivery", kind, r.Error)
		}
	}
	opened := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: v.ParticipationID})
	if !opened.OK {
		t.Fatal(opened.Error)
	}
	panel := opened.Result.(ChatGPTView)
	for _, stage := range []string{"panel_status", "pause", "renew", "notification_attempted"} {
		u := FollowUpRequest{ParticipationID: v.ParticipationID, FollowUpUpdate: chat.FollowUpUpdate{Stage: stage, EventID: stage, PanelID: "legacy", Revision: panel.FollowUps.Revision, Delivery: "enabled"}}
		if r := chatGPTCall(t, s, session, "chatgpt.followups", u); r.OK || r.Error.Code != "incompatible_client" {
			t.Fatal("legacy widget acquired controls after another panel opened", stage, r.Error)
		}
	}
	f, err := s.ControlFollowUps("status")
	if err != nil || !f.Enabled || f.Remaining != panel.FollowUps.Remaining || f.Revision != panel.FollowUps.Revision {
		t.Fatal("rejected legacy widget changed the room policy", f, err)
	}
	u := FollowUpRequest{ParticipationID: v.ParticipationID, PanelToken: panel.PanelToken, FollowUpUpdate: chat.FollowUpUpdate{Stage: "panel_status", EventID: "current", PanelID: "current", Delivery: "enabled"}}
	if r := chatGPTCall(t, s, session, "chatgpt.followups", u); !r.OK {
		t.Fatal("current panel could not enable delivery", r.Error)
	}
}
