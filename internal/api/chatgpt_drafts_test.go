//go:build !windows

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func draftServiceFixture() (*Service, *fakeController, ChatGPTDraftRequest) {
	f := newFakeController()
	now := time.Now().UTC()
	f.messages = []chat.Message{{Sequence: 2, Author: chat.ChatGPT, Kind: chat.MessageText, Text: "request"}}
	f.room.Conversations = []chat.ConversationJob{{ID: "reply", SourceSequence: 2, RemoteMessageID: "remote", State: chat.ConversationFailed, Requested: []chat.Participant{chat.Codex}, Attempts: []chat.ConversationAttempt{{Participant: chat.Codex, TurnID: "turn"}}}}
	f.room.TurnHistory = []chat.TurnRecord{{ID: "turn", Participant: chat.Codex, State: chat.TurnRecordInterrupted, Drafts: []string{"é猫🙂abc\n<!-- mohuddle:{\"hidden\":true} -->", "second segment"}, Tools: []string{"private tool log"}, StartedAt: now, CompletedAt: now, DraftCaptureVersion: 1, DraftTruncated: true}}
	s := &Service{controller: f}
	s.chatgpt.participation = "participation"
	s.chatgpt.lease = now.Add(time.Hour)
	s.chatgpt.delivered = 2
	return s, f, ChatGPTDraftRequest{ParticipationID: "participation", ReplyID: "reply", Limit: 2}
}

func TestReplyDraftPaginationAndOwnership(t *testing.T) {
	s, f, input := draftServiceFixture()
	call := func(v ChatGPTDraftRequest) Response {
		return s.readReplyDraftLocked(request(t, "id", "chatgpt.read_reply_draft", v)).Response
	}
	r := call(input)
	if !r.OK {
		t.Fatal(r.Error)
	}
	page := r.Result.(ChatGPTDraft)
	if page.Text != "é猫" || !page.Incomplete || !page.HasMore || page.CaptureTruncated == nil || !*page.CaptureTruncated {
		t.Fatalf("wrong first page: %+v", page)
	}
	text := page.Text
	for page.HasMore {
		input.DraftID = page.DraftID
		input.Segment = page.NextSegment
		input.Offset = page.NextOffset
		r = call(input)
		if !r.OK {
			t.Fatal(r.Error)
		}
		page = r.Result.(ChatGPTDraft)
		text += page.Text
	}
	if !strings.Contains(text, "🙂abc") || !strings.Contains(text, "second segment") || strings.Contains(text, "hidden") {
		t.Fatal("bad sanitized pagination", text)
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "tool log") || strings.Contains(string(data), "SessionID") {
		t.Fatal("private metadata escaped")
	}
	input.Segment = 0
	input.Offset = 0
	for _, change := range []func(*ChatGPTDraftRequest){func(v *ChatGPTDraftRequest) { v.ParticipationID = "wrong" }, func(v *ChatGPTDraftRequest) { v.DraftID = "unrelated" }, func(v *ChatGPTDraftRequest) { v.Offset = 9999 }, func(v *ChatGPTDraftRequest) { v.Limit = 16001 }} {
		bad := input
		change(&bad)
		if call(bad).OK {
			t.Fatal("invalid draft access accepted")
		}
	}
	s.chatgpt.delivered = 1
	if call(input).OK {
		t.Fatal("unread source accepted")
	}
	s.chatgpt.delivered = 2
	f.room.Conversations[0].State = chat.ConversationAnswered
	if call(input).OK {
		t.Fatal("successful turn exposed as failed draft")
	}
	f.room.Conversations[0].State = chat.ConversationFailed
	f.messages[0].Author = chat.User
	if call(input).OK {
		t.Fatal("human reply draft exposed")
	}
}

func TestReplyDraftLegacyLinksEvictionAndAmbiguity(t *testing.T) {
	s, f, _ := draftServiceFixture()
	job := f.room.Conversations[0]
	job.Attempts = nil
	if len(replyDrafts(f.room, f.messages, job)) != 0 {
		t.Fatal("guessed from participant or timestamps")
	}
	f.messages = append(f.messages, chat.Message{Sequence: 3, Author: chat.Codex, ConversationID: "reply", TurnID: "turn", Kind: chat.MessageTool})
	f.room.TurnHistory[0].DraftCaptureVersion = 0
	if len(replyDrafts(f.room, f.messages, job)) != 1 {
		t.Fatal("legacy link missing")
	}
	f.room.Conversations[0] = job
	r := s.readReplyDraftLocked(request(t, "id", "chatgpt.read_reply_draft", ChatGPTDraftRequest{ParticipationID: "participation", ReplyID: "reply"})).Response
	if !r.OK || r.Result.(ChatGPTDraft).CaptureTruncated != nil {
		t.Fatal("legacy completeness fabricated")
	}
	f.messages = append(f.messages, chat.Message{Sequence: 4, Author: chat.Codex, ConversationID: "another", TurnID: "turn", Kind: chat.MessageTool})
	if len(replyDrafts(f.room, f.messages, job)) != 0 {
		t.Fatal("ambiguous legacy link accepted")
	}
	f.room.TurnHistory = nil
	if len(replyDrafts(f.room, f.messages, job)) != 0 {
		t.Fatal("evicted draft advertised")
	}
}

type overflowingReplyAgent struct{}

func (overflowingReplyAgent) Participant() chat.Participant { return chat.Codex }
func (overflowingReplyAgent) Close() error                  { return nil }
func (overflowingReplyAgent) Run(_ context.Context, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	emit(agent.Event{Type: agent.EventDelta, Text: "Retained useful draft"})
	return agent.TurnResult{}, &agent.EventQueueOverflowError{}
}

func TestChatGPTFailedReplyCanRecoverWithoutAnotherExchange(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil, overflowingReplyAgent{})
	joined := joinChatGPT(t, s, session)
	response := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: joined.ParticipationID, OperationID: "draft-request", Text: "Inspect this", RequestReplies: []chat.Participant{chat.Codex}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	var reply ChatGPTReply
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: joined.ParticipationID})
		view := r.Result.(ChatGPTView)
		if len(view.ReplyResults) > 0 && view.ReplyResults[0].DraftAvailable {
			reply = view.ReplyResults[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reply.ID == "" || reply.ReasonCode != chat.ReasonEventQueueOverflow {
		t.Fatalf("missing draft/cause: %+v", reply)
	}
	s.chatgptMu.Lock()
	before := s.chatgpt.exchanges
	s.chatgptMu.Unlock()
	input := ChatGPTDraftRequest{ParticipationID: joined.ParticipationID, ReplyID: reply.ID}
	r := chatGPTCall(t, s, session, "chatgpt.read_reply_draft", input)
	if !r.OK || r.Result.(ChatGPTDraft).Text != "Retained useful draft" {
		t.Fatalf("recovery failed: %+v", r)
	}
	s.chatgptMu.Lock()
	after := s.chatgpt.exchanges
	s.chatgptMu.Unlock()
	if before != after {
		t.Fatal("recovery spent an exchange")
	}
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	if chatGPTCall(t, s, session, "chatgpt.read_reply_draft", input).OK {
		t.Fatal("expired lease accepted")
	}
	_ = s.RevokeChatGPT()
	if chatGPTCall(t, s, session, "chatgpt.read_reply_draft", input).OK {
		t.Fatal("revoked grant accepted")
	}
}
