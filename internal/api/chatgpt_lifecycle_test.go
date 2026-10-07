//go:build !windows

package api

import (
	"context"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type controlledChatGPTReply struct {
	chatGPTTestAgent
	started chan struct{}
	finish  chan struct{}
}

func (a controlledChatGPTReply) Run(ctx context.Context, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	emit(agent.Event{Type: agent.EventDelta, Text: "Retained public progress"})
	close(a.started)
	select {
	case <-a.finish:
		return agent.TurnResult{Text: "Completed after reconnect", Done: true}, nil
	case <-ctx.Done():
		return agent.TurnResult{}, ctx.Err()
	}
}

func TestChatGPTRejoinCollectsReplyAcceptedBeforeLeaseExpiry(t *testing.T) {
	peer := controlledChatGPTReply{started: make(chan struct{}), finish: make(chan struct{})}
	s, _, _, session := chatGPTService(t, nil, peer)
	view := joinChatGPT(t, s, session)
	response := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "accepted", Text: "Review", RequestReplies: []chat.Participant{chat.Codex}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	select {
	case <-peer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("reply not started")
	}
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.updateChatGPTStateLocked()
	s.chatgptMu.Unlock()
	response = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID})
	if response.OK || response.Error.Code != "not_joined" {
		t.Fatal("expired participation still usable")
	}
	previous := view.ParticipationID
	view = joinChatGPT(t, s, session)
	if view.ParticipationID != previous || len(view.Replies) != 1 {
		t.Fatalf("rejoin lost accepted reply: %+v", view.Replies)
	}
	close(peer.finish)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID})
		if !response.OK {
			t.Fatal(response.Error)
		}
		view = response.Result.(ChatGPTView)
		if len(view.ReplyResults) == 1 {
			result := view.ReplyResults[0]
			if result.State != chat.ConversationAnswered || result.AnswerSequence == 0 || result.CompletedAt == nil {
				t.Fatalf("reply failed: %+v", result)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("accepted reply never completed")
}

func TestChatGPTExplicitLeaveRecordsCauseAndPartialAvailability(t *testing.T) {
	peer := controlledChatGPTReply{started: make(chan struct{}), finish: make(chan struct{})}
	s, o, _, session := chatGPTService(t, nil, peer)
	view := joinChatGPT(t, s, session)
	response := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "leave", Text: "Review", RequestReplies: []chat.Participant{chat.Codex}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	select {
	case <-peer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("reply not started")
	}
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	response = chatGPTCall(t, s, session, "chatgpt.leave", ChatGPTLeaveRequest{ParticipationID: view.ParticipationID})
	if !response.OK {
		t.Fatal(response.Error)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs := o.ConversationJobs()
		if len(jobs) == 1 && jobs[0].HasPartialResponse {
			if jobs[0].ReasonCode != chat.ReasonChatGPTLeft || jobs[0].CompletedAt == nil {
				t.Fatalf("missing explicit cause: %+v", jobs[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("partial output not retained")
}

func TestExplicitTransferPreservesAcceptedReplyAndPause(t *testing.T) {
	peer := controlledChatGPTReply{started: make(chan struct{}), finish: make(chan struct{})}
	s, _, _, session := chatGPTService(t, nil, peer)
	before := joinChatGPT(t, s, session)
	r := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: before.ParticipationID, OperationID: "transfer-work", Text: "Review", RequestReplies: []chat.Participant{chat.Codex}})
	if !r.OK {
		t.Fatal(r.Error)
	}
	select {
	case <-peer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("peer did not start")
	}
	if _, err := s.ControlFollowUps("off"); err != nil {
		t.Fatal(err)
	}
	r = chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "another-conversation"})
	if r.OK || r.Error.Code != "already_joined" {
		t.Fatal("unrequested takeover allowed", r.Error)
	}
	r = chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "another-conversation", ReplaceExisting: true})
	if !r.OK {
		t.Fatal(r.Error)
	}
	after := r.Result.(ChatGPTView)
	if after.ParticipationID == before.ParticipationID || after.FollowUps.Enabled || after.State.ExchangesRemaining != before.State.ExchangesRemaining-1 || len(after.Replies) != 1 {
		t.Fatal("transfer reset pause, budget, or accepted reply")
	}
	if chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: before.ParticipationID}).OK {
		t.Fatal("old participation retained control")
	}
	close(peer.finish)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: after.ParticipationID})
		if !r.OK {
			t.Fatal(r.Error)
		}
		v := r.Result.(ChatGPTView)
		if len(v.ReplyResults) == 1 && v.ReplyResults[0].State == chat.ConversationAnswered {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transfer cancelled or lost the accepted reply")
}

func TestNewestPanelRetiresOlderPanelWithoutEndingParticipation(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil)
	joined := joinChatGPT(t, s, session)
	opened := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: joined.ParticipationID})
	if !opened.OK {
		t.Fatal(opened.Error)
	}
	first := opened.Result.(ChatGPTView)
	if again := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: first.ParticipationID}); again.OK || again.Error.Code != "panel_exists" {
		t.Fatal("unrequested replacement accepted", again.Error)
	}
	r := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: first.ParticipationID, ReplaceExisting: true})
	if !r.OK {
		t.Fatal(r.Error)
	}
	next := r.Result.(ChatGPTView)
	if next.PanelToken == "" || next.PanelToken == first.PanelToken || next.ParticipationID != first.ParticipationID {
		t.Fatal("opening panel did not replace only the panel attachment")
	}
	for _, op := range []struct {
		kind    string
		payload any
	}{
		{"chatgpt.read", ChatGPTReadRequest{ParticipationID: first.ParticipationID, PanelToken: first.PanelToken}},
		{"chatgpt.followups", FollowUpRequest{ParticipationID: first.ParticipationID, PanelToken: first.PanelToken, FollowUpUpdate: chat.FollowUpUpdate{EventID: "old-panel", Stage: "panel_status", PanelID: "old-panel", Delivery: "enabled"}}},
		{"chatgpt.notification", NotificationRequest{ParticipationID: first.ParticipationID, PanelToken: first.PanelToken, EventID: "old-panel", Stage: "panel_status"}},
	} {
		r := chatGPTCall(t, s, session, op.kind, op.payload)
		if r.OK || r.Error.Code != "panel_superseded" {
			t.Fatal("obsolete panel operation accepted", op.kind, r.Error)
		}
	}
	for _, token := range []string{"", next.PanelToken} {
		if !chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: first.ParticipationID, PanelToken: token}).OK {
			t.Fatal("model or latest panel lost access")
		}
	}
}

func TestChatGPTPanelSurvivesRejoinAndRecoversLeaseWithoutReplacement(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil)
	joined := joinChatGPT(t, s, session)
	if joined.PanelToken != "" || joined.PanelState != "not_opened" {
		t.Fatal("join allocated panel", joined.PanelState)
	}
	r := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: joined.ParticipationID})
	if !r.OK {
		t.Fatal(r.Error)
	}
	opened := r.Result.(ChatGPTView)
	again := joinChatGPT(t, s, session)
	if again.PanelToken != opened.PanelToken || again.ParticipationID != opened.ParticipationID || again.PanelState != "opened" {
		t.Fatal("rejoin replaced panel")
	}
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	input := ChatGPTReadRequest{ParticipationID: opened.ParticipationID, PanelToken: opened.PanelToken}
	r = chatGPTCall(t, s, session, "chatgpt.read", input)
	if r.OK || r.Error.Code != "participation_expired" {
		t.Fatal("expired current panel must be recoverable", r.Error)
	}
	again = joinChatGPT(t, s, session)
	if again.ParticipationID != opened.ParticipationID || again.PanelToken != opened.PanelToken {
		t.Fatal("lease renewal replaced panel")
	}
	if r = chatGPTCall(t, s, session, "chatgpt.read", input); !r.OK {
		t.Fatal("refresh could not recover", r.Error)
	}
	if r = chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "replacement-conversation", ReplaceExisting: true}); !r.OK {
		t.Fatal(r.Error)
	}
	if r = chatGPTCall(t, s, session, "chatgpt.read", input); r.OK {
		t.Fatal("transferred panel regained access")
	}
}

func TestExpiredPanelOperationsRemainRecoverableOnlyForCurrentAttachment(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil)
	joined := joinChatGPT(t, s, session)
	r := chatGPTCall(t, s, session, "chatgpt.panel", ChatGPTPanelRequest{ParticipationID: joined.ParticipationID})
	if !r.OK {
		t.Fatal(r.Error)
	}
	opened := r.Result.(ChatGPTView)
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	for _, attachment := range []struct {
		name, participation, token string
		recoverable                bool
	}{
		{"current", opened.ParticipationID, opened.PanelToken, true},
		{"model", opened.ParticipationID, "", false},
		{"obsolete-panel", opened.ParticipationID, "obsolete", false},
		{"other-participation", "other", opened.PanelToken, false},
	} {
		for _, kind := range []string{"chatgpt.read", "chatgpt.followups", "chatgpt.notification"} {
			t.Run(attachment.name+"/"+kind, func(t *testing.T) {
				payload := map[string]any{"participation_id": attachment.participation, "panel_token": attachment.token}
				if kind != "chatgpt.read" {
					payload["event_id"], payload["panel_id"], payload["stage"] = "heartbeat", "panel", "panel_status"
				}
				result := chatGPTCall(t, s, session, kind, payload)
				if result.OK || (result.Error.Code == "participation_expired") != attachment.recoverable {
					t.Fatalf("unexpected recovery permission: %+v", result.Error)
				}
			})
		}
	}
	again := joinChatGPT(t, s, session)
	if again.ParticipationID != opened.ParticipationID || again.PanelToken != opened.PanelToken {
		t.Fatal("recovery replaced attachment")
	}
	r = chatGPTCall(t, s, session, "chatgpt.followups", FollowUpRequest{ParticipationID: opened.ParticipationID, PanelToken: opened.PanelToken, FollowUpUpdate: chat.FollowUpUpdate{EventID: "recovered", PanelID: "panel", Stage: "panel_status", Delivery: "enabled"}})
	if !r.OK {
		t.Fatal("heartbeat could not recover after rejoin", r.Error)
	}
}
