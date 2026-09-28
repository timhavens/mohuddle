//go:build !windows

package api

import (
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
	roomguidance "github.com/timhavens/mohuddle/internal/chatgpt/skills/mohuddle-room"
)

func TestFollowUpsAPIWorksWithoutMonitorAndPreservesRejoin(t *testing.T) {
	s, o, session := handoffService(t)
	v := joinChatGPT(t, s, session)
	if !v.FollowUps.Enabled || v.ToolContractVersion != roomguidance.ToolContractVersion || !strings.Contains(v.ClientCompatibility, "unknown") {
		t.Fatal(v)
	}
	r := chatGPTCall(t, s, session, "chatgpt.followups", FollowUpRequest{ParticipationID: v.ParticipationID, FollowUpUpdate: chat.FollowUpUpdate{Stage: "pause", EventID: "pause", Revision: v.FollowUps.Revision}})
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
	r = chatGPTCall(t, s, session, "chatgpt.followups", FollowUpRequest{ParticipationID: v.ParticipationID, FollowUpUpdate: chat.FollowUpUpdate{Stage: "resume", EventID: "stale", Revision: 1}})
	if r.OK {
		t.Fatal("stale control accepted")
	}
	u := FollowUpRequest{ParticipationID: v.ParticipationID, FollowUpUpdate: chat.FollowUpUpdate{Stage: "resume", EventID: "resume", Revision: v.FollowUps.Revision}}
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
		t.Fatal(r)
	}
	u.Stage = "host_accepted"
	if r = chatGPTCall(t, s, session, "chatgpt.followups", u); !r.OK || r.Result.(chat.FollowUpView).LastOutcome != "host_accepted" {
		t.Fatal(r)
	}
	if v.Coordination != nil {
		t.Fatal("follow-ups must not require or start a monitored run")
	}
}
