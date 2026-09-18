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
	if view.ParticipationID == previous || len(view.Replies) != 1 {
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
