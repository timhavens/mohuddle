//go:build !windows

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

type chatGPTTestAgent struct{}

func (chatGPTTestAgent) Participant() chat.Participant { return chat.Codex }
func (chatGPTTestAgent) Close() error                  { return nil }
func (chatGPTTestAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: "Shared peer reply", Done: true}, nil
}

func chatGPTService(t *testing.T, messages []chat.Message, peers ...agent.Agent) (*Service, *room.Orchestrator, ChatGPTConnection, *Session) {
	t.Helper()
	root := testutil.ShortTempDir(t)
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) == 0 {
		peers = []agent.Agent{chatGPTTestAgent{}}
	}
	o, err := room.New(r, messages, s, peers...)
	if err != nil {
		t.Fatal(err)
	}
	o.ConfigureTemporaryAgents(nil)
	credentials, err := LoadOrCreateCredentials(CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	server, err := StartLocal(filepath.Join(root, "api.sock"), service, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureChatGPT(server.Addr(), filepath.Join(root, "chatgpt.json"), NewAuditLog(filepath.Join(root, "audit.jsonl")))
	path, err := service.EnableChatGPT(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := ReadChatGPTConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(HelloRequest{ClientID: "test", Token: connection.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.RevokeChatGPT(); _ = server.Close(); _ = o.Close() })
	return service, o, connection, session
}

func chatGPTCall(t *testing.T, s *Service, session *Session, kind string, payload any) Response {
	t.Helper()
	return s.Handle(context.Background(), session, request(t, "operation", kind, payload)).Response
}

func joinChatGPT(t *testing.T, s *Service, session *Session) ChatGPTView {
	t.Helper()
	r := chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "conversation-one"})
	if !r.OK {
		t.Fatalf("join failed: %+v", r.Error)
	}
	return r.Result.(ChatGPTView)
}

func TestChatGPTRoundRequiresParticipationAndConsumesOneExchange(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	input := ChatGPTRoundRequest{OperationID: "one-round", Text: "Review the existing proposal"}
	response := chatGPTCall(t, s, session, "chatgpt.request_round", input)
	if response.OK || response.Error.Code != "not_joined" {
		t.Fatal("round did not require participation")
	}
	view := joinChatGPT(t, s, session)
	input.ParticipationID = view.ParticipationID
	for _, payload := range []any{
		map[string]any{"participation_id": view.ParticipationID, "operation_id": "unknown-control", "text": "Review", "permissions": "full"},
		ChatGPTRoundRequest{ParticipationID: view.ParticipationID, OperationID: "bad-target", Text: "Review", Participants: []chat.Participant{chat.ChatGPT}},
		ChatGPTRoundRequest{ParticipationID: view.ParticipationID, OperationID: "unread-draft", Text: "Review", ReplyTo: 999},
	} {
		if response := chatGPTCall(t, s, session, "chatgpt.request_round", payload); response.OK {
			t.Fatal("invalid round accepted")
		}
	}
	_, messages := o.Snapshot()
	if len(messages) != 0 {
		t.Fatal("invalid round was posted")
	}
	response = chatGPTCall(t, s, session, "chatgpt.request_round", input)
	if !response.OK {
		t.Fatalf("round failed: %+v", response.Error)
	}
	receipt := response.Result.(map[string]any)
	if receipt["action"] != "round" || receipt["exchanges_remaining"] != 31 || receipt["moderator"] != chat.Codex {
		t.Fatalf("round receipt: %+v", receipt)
	}
	duplicate := chatGPTCall(t, s, session, "chatgpt.request_round", input)
	if !duplicate.OK || duplicate.Result.(map[string]any)["exchanges_remaining"] != 31 || duplicate.Result.(map[string]any)["duplicate"] != true {
		t.Fatal("retry spent another exchange")
	}
	o.Stop()
	input.OperationID = "paused-round"
	if response := chatGPTCall(t, s, session, "chatgpt.request_round", input); response.OK || response.Error.Code != "paused" {
		t.Fatal("host stop did not pause round requests")
	}
}

type failingChatGPTTestAgent struct{ chatGPTTestAgent }

func (failingChatGPTTestAgent) Run(context.Context, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{}, fmt.Errorf("provider failed at /private/provider/session")
}

func TestChatGPTReadDistinguishesFailedRepliesFromPendingOrAgreement(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil, failingChatGPTTestAgent{})
	view := joinChatGPT(t, s, session)
	response := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "failed-peer", Text: "Review this proposal", RequestReplies: []chat.Participant{chat.Codex}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	source := response.Result.(map[string]any)["sequence"].(uint64)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter})
		if !response.OK {
			t.Fatal(response.Error)
		}
		view = response.Result.(ChatGPTView)
		for _, result := range view.ReplyResults {
			if result.SourceSequence == source && result.Participant == chat.Codex {
				if result.State != chat.ConversationFailed || len(view.Replies) != 0 {
					t.Fatalf("incorrect terminal reply: %+v", result)
				}
				data, _ := json.Marshal(view)
				if strings.Contains(string(data), "/private/provider/session") {
					t.Fatal("provider internals leaked into ChatGPT status")
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("failed reply status was not observable")
}

func TestChatGPTGrantIsPrivateRoomBoundAndRevocable(t *testing.T) {
	s, o, connection, session := chatGPTService(t, nil)
	if session.Kind != ClientChatGPT || session.Has(ScopeAdminister) {
		t.Fatal("grant is not restricted")
	}
	info, err := os.Stat(s.chatgpt.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("connection permissions: %v", err)
	}
	session.Scopes[ScopeAdminister] = true // Misconfiguration must not grant room controls.
	for _, kind := range []string{"room.join", "room.get", "history.get", "status.get", "message.send", "command.invoke", "events.subscribe"} {
		r := chatGPTCall(t, s, session, kind, map[string]any{"command": "join", "participant": "claude", "text": "change files"})
		if r.OK || r.Error.Code != "forbidden" {
			t.Fatalf("%s escaped allowlist: %+v", kind, r)
		}
	}
	joined := joinChatGPT(t, s, session)
	if joined.ParticipationID == "" {
		t.Fatal("missing participation")
	}
	duplicate := joinChatGPT(t, s, session)
	if duplicate.ParticipationID != joined.ParticipationID {
		t.Fatal("join retry replaced participation")
	}
	other := chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "conversation-two"})
	if other.OK || other.Error.Code != "already_joined" {
		t.Fatal("second conversation took over")
	}
	wrong := request(t, "wrong-room", "chatgpt.read", ChatGPTReadRequest{ParticipationID: joined.ParticipationID})
	wrong.RoomID = "another-room"
	if s.Handle(context.Background(), session, wrong).Response.OK {
		t.Fatal("cross-room request allowed")
	}
	if err := s.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(HelloRequest{ClientID: "test", Token: connection.Token}); err == nil {
		t.Fatal("revoked token authenticated")
	}
	if chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: joined.ParticipationID}).OK {
		t.Fatal("existing session survived revocation")
	}
	state, _ := o.Snapshot()
	if state.Present(chat.ChatGPT) {
		t.Fatal("revoked peer still present")
	}
	if _, err := os.Stat(s.chatgpt.path); !os.IsNotExist(err) {
		t.Fatal("credential file retained")
	}
	if _, err := s.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(HelloRequest{ClientID: "test", Token: connection.Token}); err == nil {
		t.Fatal("rotation revived old token")
	}
}

func TestChatGPTHistoryIsBoundedAndDoesNotExposeHostData(t *testing.T) {
	messages := []chat.Message{
		{ID: "one", Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "Shared question", Attachments: []chat.Attachment{{Path: "/private/attachment"}}},
		{ID: "two", Sequence: 2, Author: chat.Codex, Kind: chat.MessageTool, Text: "/private/tool-secret"},
		{ID: "three", Sequence: 3, Author: chat.System, Kind: chat.MessageError, Text: "/private/error-secret"},
		{ID: "four", Sequence: 4, Author: chat.Claude, Kind: chat.MessageText, Text: strings.Repeat("x", 17000)},
	}
	s, _, connection, session := chatGPTService(t, messages)
	view := joinChatGPT(t, s, session)
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "/private/") || strings.Contains(string(data), connection.Token) || strings.Contains(string(data), connection.Socket) {
		t.Fatal("host data escaped into ChatGPT")
	}
	if len(view.Messages) != 2 || !view.Messages[1].Truncated || len(view.Messages[1].Text) != 16000 {
		t.Fatal("history was not filtered/bounded")
	}
	page := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID, Limit: 1}).Result.(ChatGPTView)
	if !page.HasMore || page.NextAfter != 3 {
		t.Fatalf("bad page cursor: %+v", page)
	}
	page = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: page.NextAfter, Limit: 1}).Result.(ChatGPTView)
	if page.HasMore || len(page.Messages) != 1 || page.Messages[0].Sequence != 4 {
		t.Fatal("pagination skipped a message")
	}
	for _, input := range []ChatGPTReadRequest{
		{ParticipationID: view.ParticipationID, After: 99}, {ParticipationID: view.ParticipationID, Limit: 101},
		{ParticipationID: view.ParticipationID, WaitSeconds: 26}, {ParticipationID: "other"},
	} {
		if chatGPTCall(t, s, session, "chatgpt.read", input).OK {
			t.Fatalf("invalid read accepted: %+v", input)
		}
	}
}

func TestChatGPTPostingIsIdempotentAndCannotRouteWork(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	view := joinChatGPT(t, s, session)
	input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "one", Text: "/permissions @all full; implement these changes"}
	first := chatGPTCall(t, s, session, "chatgpt.publish", input)
	if !first.OK {
		t.Fatalf("publish: %+v", first.Error)
	}
	second := chatGPTCall(t, s, session, "chatgpt.publish", input)
	if !second.OK || !second.Result.(map[string]any)["duplicate"].(bool) {
		t.Fatal("retry was not idempotent")
	}
	input.Text = "changed content"
	if chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("operation reused with changed payload")
	}
	state, messages := o.Snapshot()
	if len(messages) != 1 || messages[0].Author != chat.ChatGPT || messages[0].Route == nil || len(state.Workflows) != 0 || len(state.PendingRoutes) != 0 {
		t.Fatal("AI contribution was routed as human work")
	}
	o.Stop()
	input.OperationID = "two"
	if chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("stop did not pause ChatGPT")
	}
	if err := s.ResumeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if !chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("trusted resume failed")
	}
	input.OperationID = "three"
	input.ReplyTo = 99
	if chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("reply to unseen source accepted")
	}
	input.ReplyTo = 0
	input.RequestReplies = []chat.Participant{chat.ChatGPT}
	if chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("self-reply loop allowed")
	}
	if !chatGPTCall(t, s, session, "chatgpt.leave", ChatGPTLeaveRequest{view.ParticipationID}).OK {
		t.Fatal("leave failed")
	}
	if chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("departed participation could post")
	}
}

func TestChatGPTWorkRequiresCurrentParticipationAndExplicitWorkEndpoint(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	view := joinChatGPT(t, s, session)
	input := ChatGPTWorkRequest{ParticipationID: view.ParticipationID, OperationID: "delegated-work", Target: "@CODEX", Text: "Apply the requested title-only edit. No commit or push."}
	for name, changed := range map[string]ChatGPTWorkRequest{
		"wrong-participation": {ParticipationID: "other", OperationID: "invalid", Target: chat.Codex, Text: "edit"},
		"unseen-reply":        {ParticipationID: view.ParticipationID, OperationID: "invalid", Target: chat.Codex, Text: "edit", ReplyTo: 999},
		"self-target":         {ParticipationID: view.ParticipationID, OperationID: "invalid", Target: chat.ChatGPT, Text: "edit"},
		"absent-peer":         {ParticipationID: view.ParticipationID, OperationID: "invalid", Target: chat.Claude, Text: "edit"},
	} {
		if response := chatGPTCall(t, s, session, "chatgpt.request_work", changed); response.OK {
			t.Fatalf("accepted %s", name)
		}
	}
	for _, extra := range []string{"permissions", "command", "mode", "steer", "request_replies"} {
		payload := map[string]any{"participation_id": view.ParticipationID, "operation_id": "invalid-extra", "target": "codex", "text": "edit", extra: "full"}
		if chatGPTCall(t, s, session, "chatgpt.request_work", payload).OK {
			t.Fatalf("accepted control override %s", extra)
		}
	}
	first := chatGPTCall(t, s, session, "chatgpt.request_work", input)
	if !first.OK {
		t.Fatalf("work failed: %+v", first.Error)
	}
	result := first.Result.(map[string]any)
	if result["duplicate"].(bool) || result["workflow_id"] == "" {
		t.Fatal("work acceptance missing identity")
	}
	duplicate := chatGPTCall(t, s, session, "chatgpt.request_work", input)
	if !duplicate.OK || !duplicate.Result.(map[string]any)["duplicate"].(bool) || duplicate.Result.(map[string]any)["workflow_id"] != result["workflow_id"] {
		t.Fatal("API retry duplicated work")
	}
	state, messages := o.Snapshot()
	if len(state.Workflows) != 1 || messages[0].Author != chat.ChatGPT || messages[0].Target != chat.Codex {
		t.Fatal("work authority or attribution changed")
	}
	read := chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID}).Result.(ChatGPTView)
	if len(read.Work) != 1 || read.Work[0].WorkflowID != result["workflow_id"] {
		t.Fatal("work status unavailable")
	}
	o.Stop()
	input.OperationID = "after-stop"
	if response := chatGPTCall(t, s, session, "chatgpt.request_work", input); response.OK || response.Error.Code != "paused" {
		t.Fatal("work ignored host stop")
	}
	if err := s.ResumeChatGPT(); err != nil {
		t.Fatal(err)
	}
	s.chatgpt.exchanges = chat.DefaultChatGPTLimits().Exchanges
	if response := chatGPTCall(t, s, session, "chatgpt.request_work", input); response.OK || response.Error.Code != "exchange_limit" {
		t.Fatal("work bypassed exchange budget")
	}
	if err := s.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if response := chatGPTCall(t, s, session, "chatgpt.request_work", input); response.OK || response.Error.Code != "authentication_failed" {
		t.Fatal("revoked grant scheduled work")
	}
}

func TestChatGPTWaitStopsOnRevocationAndCancellation(t *testing.T) {
	for _, reason := range []string{"revoke", "cancel"} {
		t.Run(reason, func(t *testing.T) {
			s, _, _, session := chatGPTService(t, nil)
			view := joinChatGPT(t, s, session)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Response, 1)
			req := request(t, "wait", "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID, WaitSeconds: 25})
			go func() { done <- s.Handle(ctx, session, req).Response }()
			deadline := time.Now().Add(time.Second)
			waiting := false
			for time.Now().Before(deadline) {
				s.chatgptMu.Lock()
				waiting = s.chatgpt.reading
				s.chatgptMu.Unlock()
				if waiting {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !waiting {
				t.Fatal("read did not wait")
			}
			if !chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID}).OK {
				t.Fatal("live panel refresh was blocked by a model waiting for replies")
			}
			if chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID, WaitSeconds: 1}).OK {
				t.Fatal("second simultaneous waiting read was accepted")
			}
			if reason == "revoke" {
				_ = s.RevokeChatGPT()
			} else {
				cancel()
			}
			select {
			case response := <-done:
				if response.OK {
					t.Fatal("cancelled read returned room data")
				}
			case <-time.After(time.Second):
				t.Fatal("read ignored cancellation")
			}
		})
	}
}

func TestChatGPTConnectionRejectsSharedFilesAndExpiry(t *testing.T) {
	s, _, _, session := chatGPTService(t, nil)
	path := s.chatgpt.path
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChatGPTConnection(path); err == nil {
		t.Fatal("shared credential accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChatGPTConnection(link); err == nil {
		t.Fatal("symlink credential accepted")
	}
	view := joinChatGPT(t, s, session)
	s.chatgptMu.Lock()
	s.chatgpt.lease = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	if chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID}).OK {
		t.Fatal("expired lease accepted")
	}
	s.chatgptMu.Lock()
	s.chatgpt.expires = time.Now().Add(-time.Second)
	s.chatgptMu.Unlock()
	if chatGPTCall(t, s, session, "chatgpt.join", ChatGPTJoinRequest{ClientKey: "again"}).OK {
		t.Fatal("expired grant accepted")
	}
}

func TestChatGPTFollowUpsHaveHostControlledBudgetAndPostingLimit(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	limits := chat.DefaultChatGPTLimits()
	limits.Exchanges = 8
	if err := s.SetChatGPTLimits(limits); err != nil {
		t.Fatal(err)
	}
	view := joinChatGPT(t, s, session)
	for i := 0; i < limits.Exchanges; i++ {
		input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: fmt.Sprintf("exchange-%d", i), Text: fmt.Sprintf("Please investigate question %d", i), RequestReplies: []chat.Participant{chat.Codex}}
		if !chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
			t.Fatal("authorized exchange failed")
		}
		deadline := time.Now().Add(time.Second)
		settled := false
		for time.Now().Before(deadline) {
			state, _ := o.Snapshot()
			settled = true
			for _, job := range state.Conversations {
				settled = settled && job.State.Terminal()
			}
			if settled {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !settled {
			t.Fatal("peer reply did not settle")
		}
	}
	input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "over-budget", Text: "One more", RequestReplies: []chat.Participant{chat.Codex}}
	response := chatGPTCall(t, s, session, "chatgpt.publish", input)
	if response.OK || response.Error.Code != "exchange_limit" {
		t.Fatal("autonomous exchange budget bypassed")
	}
	if err := o.Post("@chatgpt Please continue discussing this result"); err != nil {
		t.Fatal(err)
	}
	response = chatGPTCall(t, s, session, "chatgpt.read", ChatGPTReadRequest{ParticipationID: view.ParticipationID})
	if !response.OK || response.Result.(ChatGPTView).State.ExchangesRemaining != limits.Exchanges {
		t.Fatal("new host message did not refresh visible budget")
	}
	if !chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
		t.Fatal("human-authorized follow-up was blocked")
	}
	input.RequestReplies = nil
	for i := 9; i < 20; i++ {
		input.OperationID = fmt.Sprintf("post-%d", i)
		if !chatGPTCall(t, s, session, "chatgpt.publish", input).OK {
			t.Fatal("bounded post was blocked")
		}
	}
	input.OperationID = "too-many"
	response = chatGPTCall(t, s, session, "chatgpt.publish", input)
	if response.OK || response.Error.Code != "rate_limited" {
		t.Fatal("posting rate limit bypassed")
	}
}
