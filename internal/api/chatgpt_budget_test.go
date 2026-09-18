//go:build !windows

package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func awaitChatGPTReplies(t *testing.T, s *Service, session *Session, participation string) ChatGPTView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: participation})
		if !response.OK {
			t.Fatal(response.Error)
		}
		view := response.Result.(ChatGPTView)
		if len(view.Replies) == 0 {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("peer replies never settled")
	return ChatGPTView{}
}

func TestChatGPTDefaultAllows32ExchangesAndPreservesRateLimit(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	// The real TUI consumes this channel. Exercise a longer session without
	// letting this headless fixture fill the UI's bounded event queue.
	go func() {
		for {
			select {
			case <-o.Events():
			case <-t.Context().Done():
				return
			}
		}
	}()
	view := joinChatGPT(t, s, session)
	if view.State.ExchangesRemaining != 32 || view.State.Limits != chat.DefaultChatGPTLimits() {
		t.Fatalf("defaults: %+v", view.State)
	}
	for _, payload := range []map[string]any{
		{"participation_id": view.ParticipationID, "limits": map[string]int{"exchanges": 1000}},
		{"participation_id": view.ParticipationID, "exchanges": 1000},
	} {
		if response := chatGPTCall(t, s, session, "chatgpt.limits", payload); response.OK {
			t.Fatal("external peer changed host limits")
		}
	}
	var input ChatGPTPublishRequest
	for i := 0; i < 32; i++ {
		input = ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: fmt.Sprintf("request-%d", i), Text: fmt.Sprintf("Investigate question %d", i), RequestReplies: []chat.Participant{chat.Codex}}
		if i == 20 {
			response := chatGPTCall(t, s, session, "chatgpt.publish", input)
			if response.OK || response.Error.Code != "rate_limited" {
				t.Fatal("larger budget bypassed rate limit")
			}
			// Advance only the server's rate window, without slowing this test.
			s.chatgptMu.Lock()
			s.chatgpt.window = time.Now().Add(-time.Minute)
			s.chatgptMu.Unlock()
		}
		if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK {
			t.Fatal(response.Error)
		}
		view = awaitChatGPTReplies(t, s, session, view.ParticipationID)
		if view.State.ExchangesRemaining != 31-i {
			t.Fatalf("remaining=%d after request %d", view.State.ExchangesRemaining, i)
		}
	}
	if view.State.PauseReason != "exchange_limit" || view.State.Paused {
		t.Fatalf("budget confused with host stop: %+v", view.State)
	}
	// A lost acceptance can still be retried at the limit without redispatch.
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK || response.Result.(map[string]any)["duplicate"] != true {
		t.Fatal("idempotent retry blocked at limit")
	}
	input.OperationID = "over-limit"
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); response.OK || response.Error.Code != "exchange_limit" {
		t.Fatal("budget not enforced")
	}
	input.RequestReplies = nil
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK {
		t.Fatal("summary blocked by exchange budget")
	}
}

func TestChatGPTRepetitionPausePreservesResultsAndNeedsHostResume(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil, failingChatGPTTestAgent{})
	view := joinChatGPT(t, s, session)
	input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, Text: "Inspect this result", RequestReplies: []chat.Participant{chat.Codex}}
	for i := 0; i < 3; i++ {
		input.OperationID = fmt.Sprintf("attempt-%d", i)
		if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK {
			t.Fatal(response.Error)
		}
		view = awaitChatGPTReplies(t, s, session, view.ParticipationID)
	}
	input.OperationID, input.Text = "another-id", "Inspect  this\nresult"
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); response.OK || response.Error.Code != "no_progress" {
		t.Fatalf("loop not stopped: %+v", response)
	} else if receipt := response.Result.(map[string]any); receipt["exchanges_remaining"] != 29 || receipt["pause_reason"] != "no_progress" || receipt["message_posted"] != false {
		t.Fatalf("pause receipt misreported remaining budget: %+v", receipt)
	}
	view = joinChatGPT(t, s, session)
	if view.State.PauseReason != "no_progress" || view.State.ExchangesRemaining != 29 || len(view.ReplyResults) != 3 {
		t.Fatalf("rejoin reset pause or lost results: %+v", view)
	}
	input.Text = "Different instruction"
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); response.OK || response.Error.Code != "no_progress" {
		t.Fatal("changed text escaped active loop pause")
	}
	input.RequestReplies = nil
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK {
		t.Fatal("loop pause blocked summary")
	}
	if err := s.ResumeChatGPT(); err != nil {
		t.Fatal(err)
	}
	state, _ := s.ChatGPTStatus()
	if state.PauseReason != "" || state.ExchangesRemaining != 32 {
		t.Fatal("resume did not replenish authorization")
	}
	// Local human direction refreshes the budget too, but never overrides /stop.
	s.chatgptMu.Lock()
	s.chatgpt.noProgress = true
	s.chatgpt.exchanges = 32
	s.chatgptMu.Unlock()
	if err := o.Post("@chatgpt Continue with the corrected scope"); err != nil {
		t.Fatal(err)
	}
	state, _ = s.ChatGPTStatus()
	if state.PauseReason != "" || state.ExchangesRemaining != 32 {
		t.Fatal("host direction did not refresh budget")
	}
	o.Stop()
	limits := chat.DefaultChatGPTLimits()
	limits.Exchanges = 64
	if err := s.SetChatGPTLimits(limits); err != nil {
		t.Fatal(err)
	}
	state, _ = s.ChatGPTStatus()
	if !state.Paused || state.PauseReason != "host_paused" {
		t.Fatal("limit change bypassed host pause")
	}
}

func TestChatGPTBudgetPauseDoesNotCancelAcceptedReply(t *testing.T) {
	peer := controlledChatGPTReply{started: make(chan struct{}), finish: make(chan struct{})}
	s, _, _, session := chatGPTService(t, nil, peer)
	limits := chat.DefaultChatGPTLimits()
	limits.Exchanges = 1
	if err := s.SetChatGPTLimits(limits); err != nil {
		t.Fatal(err)
	}
	view := joinChatGPT(t, s, session)
	input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "accepted", Text: "Review", RequestReplies: []chat.Participant{chat.Codex}}
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); !response.OK {
		t.Fatal(response.Error)
	}
	select {
	case <-peer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("not started")
	}
	input.OperationID = "blocked"
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); response.OK || response.Error.Code != "exchange_limit" {
		t.Fatal("new request not blocked")
	}
	read := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID}).Result.(ChatGPTView)
	if len(read.Replies) != 1 || read.State.PauseReason != "exchange_limit" {
		t.Fatal("accepted reply was cancelled or budget invisible")
	}
	limits.Exchanges = 5
	if err := s.SetChatGPTLimits(limits); err != nil {
		t.Fatal(err)
	}
	state, _ := s.ChatGPTStatus()
	if state.ExchangesRemaining != 4 || state.PauseReason != "" {
		t.Fatal("editing limit reset usage")
	}
	close(peer.finish)
	read = awaitChatGPTReplies(t, s, session, view.ParticipationID)
	if len(read.ReplyResults) != 1 || read.ReplyResults[0].State != chat.ConversationAnswered {
		t.Fatalf("accepted reply did not complete: %+v", read.ReplyResults)
	}
	if err := s.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	state, _ = s.ChatGPTStatus()
	if state.Limits != limits || state.ExchangesRemaining != 5 {
		t.Fatal("revoking/renewing lost room settings")
	}
}

func TestChatGPTProgressUsesDistinctPeerTextAndSuccessfulWork(t *testing.T) {
	a := chatGPTAccess{}
	key := chatGPTRequestFingerprint("chatgpt.publish", "Review", []chat.Participant{chat.Codex})
	a.repeated = map[[32]byte]int{key: 3}
	message := chat.Message{Sequence: 1, Author: chat.Codex, Kind: chat.MessageText, Text: "New evidence"}
	a.observeProgress(chat.Room{}, []chat.Message{message})
	if a.repeated[key] != 0 {
		t.Fatal("new evidence did not allow follow-up")
	}
	a.repeated = map[[32]byte]int{key: 3}
	message.Sequence = 2
	a.observeProgress(chat.Room{}, []chat.Message{message})
	if a.repeated[key] != 3 {
		t.Fatal("identical answer counted as progress")
	}
	message.Sequence, message.Text = 3, "Different evidence"
	a.observeProgress(chat.Room{}, []chat.Message{message})
	if a.repeated[key] != 0 {
		t.Fatal("distinct answer not recognized")
	}
	a.repeated = map[[32]byte]int{key: 3}
	source := chat.Message{Sequence: 4, Author: chat.ChatGPT, Kind: chat.MessageText, InputIntent: chat.InputWork, WorkflowID: "work", Route: &chat.RouteMetadata{MessageID: "work"}}
	state := chat.Room{Workflows: map[string]chat.WorkflowRecord{"work": {ID: "work", State: chat.WorkflowCancelled}}}
	a.observeProgress(state, []chat.Message{source})
	if a.repeated[key] != 3 {
		t.Fatal("failed work treated as progress")
	}
	state.Workflows["work"] = chat.WorkflowRecord{ID: "work", State: chat.WorkflowCompleted}
	a.observeProgress(state, []chat.Message{source})
	if a.repeated[key] != 0 {
		t.Fatal("successful work not treated as progress")
	}
	a.repeated = map[[32]byte]int{key: 3}
	a.observeProgress(state, []chat.Message{source})
	if a.repeated[key] != 3 {
		t.Fatal("polling old completion treated as new progress")
	}
}
