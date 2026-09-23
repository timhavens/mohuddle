//go:build !windows

package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestChatGPTResearchDeadlineAndIdempotency(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	v := joinChatGPT(t, s, session)
	for i, class := range []chat.ConversationClass{"", chat.ConversationResearch} {
		input := ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: fmt.Sprintf("class-%d", i), Text: fmt.Sprintf("Inspect group %d", i), RequestReplies: []chat.Participant{chat.Codex}, ReplyClass: class}
		r := chatGPTCall(t, s, session, "chatgpt.publish", input)
		if !r.OK {
			t.Fatal(r.Error)
		}
		v = awaitChatGPTReplies(t, s, session, v.ParticipationID)
		state, _ := o.Snapshot()
		job := state.Conversations[len(state.Conversations)-1]
		want := 10 * time.Minute
		if class == chat.ConversationResearch {
			want = 30 * time.Minute
		}
		if job.Deadline.Sub(job.CreatedAt) != want {
			t.Fatalf("deadline %v want %v", job.Deadline.Sub(job.CreatedAt), want)
		}
		if r := chatGPTCall(t, s, session, "chatgpt.publish", input); !r.OK || r.Result.(map[string]any)["duplicate"] != true {
			t.Fatal("retry failed", r.Error)
		}
		if class == "" {
			input.ReplyClass = chat.ConversationResearch
		} else {
			input.ReplyClass = chat.ConversationQuick
		}
		if r := chatGPTCall(t, s, session, "chatgpt.publish", input); r.OK {
			t.Fatal("changed class reused operation")
		}
	}
	before, _ := o.Snapshot()
	for _, input := range []ChatGPTPublishRequest{
		{ParticipationID: v.ParticipationID, OperationID: "invalid", Text: "read", ReplyClass: "forever", RequestReplies: []chat.Participant{chat.Codex}},
		{ParticipationID: v.ParticipationID, OperationID: "text-only", Text: "read", ReplyClass: chat.ConversationResearch},
	} {
		if r := chatGPTCall(t, s, session, "chatgpt.publish", input); r.OK {
			t.Fatal("invalid class accepted")
		}
	}
	after, _ := o.Snapshot()
	if len(before.Conversations) != len(after.Conversations) {
		t.Fatal("invalid request dispatched")
	}
}

func TestChatGPTCompleteMessagePagesAreVersionedAndUnicodeSafe(t *testing.T) {
	full := strings.Repeat("é猫🙂", 9000)
	s, _, _, session := chatGPTService(t, []chat.Message{{ID: "public", Sequence: 1, Author: chat.Codex, Kind: chat.MessageText, Text: full}, {ID: "tool", Sequence: 2, Author: chat.Codex, Kind: chat.MessageTool, Text: "private tool"}})
	v := joinChatGPT(t, s, session)
	if !v.Messages[0].Truncated {
		t.Fatal("fixture did not require paging")
	}
	request := ChatGPTMessageRequest{ParticipationID: v.ParticipationID, Sequence: 1, Limit: 7000}
	var result strings.Builder
	for {
		r := chatGPTCall(t, s, session, "chatgpt.read_message", request)
		if !r.OK {
			t.Fatal(r.Error)
		}
		p := r.Result.(ChatGPTMessagePage)
		result.WriteString(p.Text)
		if p.Truncated {
			t.Fatal("page silently truncated")
		}
		if !p.HasMore {
			if !p.Complete {
				t.Fatal("last page not marked complete")
			}
			break
		}
		request.Offset, request.SHA256 = p.NextOffset, p.SHA256
	}
	if result.String() != full {
		t.Fatal("paging lost Unicode text")
	}
	request.SHA256 = "wrong"
	if r := chatGPTCall(t, s, session, "chatgpt.read_message", request); r.OK || r.Error.Code != "version_mismatch" {
		t.Fatal("wrong version accepted", r.Error)
	}
	request.Sequence = 2
	request.Offset = 0
	request.SHA256 = ""
	if r := chatGPTCall(t, s, session, "chatgpt.read_message", request); r.OK {
		t.Fatal("tool output exposed")
	}
	request.Sequence = 99
	if r := chatGPTCall(t, s, session, "chatgpt.read_message", request); r.OK {
		t.Fatal("undelivered source accepted")
	}
	request.Sequence = 1
	request.ParticipationID = "another-room"
	if r := chatGPTCall(t, s, session, "chatgpt.read_message", request); r.OK {
		t.Fatal("foreign participation accepted")
	}
}

func TestChatGPTMonitoredRunDoesNotResumeOnAccessRenewal(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	v := joinChatGPT(t, s, session)
	monitor, err := s.ControlCoordination("start")
	if err != nil {
		t.Fatal(err)
	}
	r := chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "group-one", Text: "Inspect this group", RequestReplies: []chat.Participant{chat.Codex}, ReplyClass: chat.ConversationResearch})
	if !r.OK {
		t.Fatal(r.Error)
	}
	v = awaitChatGPTReplies(t, s, session, v.ParticipationID)
	if v.Coordination == nil || v.Coordination.LastResultAt.IsZero() {
		t.Fatal("completed assignment missing")
	}
	if !v.Coordination.AcknowledgedAt.IsZero() {
		t.Fatal("read counted as acknowledgement")
	}
	var resultID string
	for _, event := range v.Coordination.Events {
		if event.Kind == "result_available" {
			resultID = event.ResultID
		}
	}
	report := CoordinatorReportRequest{ParticipationID: v.ParticipationID, RunID: monitor.ID, EventID: "ack-one", ResultID: resultID, State: "pending", Detail: "Independent review next"}
	if r := chatGPTCall(t, s, session, "chatgpt.coordinator_report", report); !r.OK {
		t.Fatal(r.Error)
	}
	state, _ := o.Snapshot()
	if len(state.Conversations) != 1 {
		t.Fatal("report auto-dispatched review")
	}
	o.Stop()
	path, err := s.EnableChatGPT(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ReadChatGPTConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err = s.Authenticate(HelloRequest{ClientID: "renewed", Token: conn.Token})
	if err != nil {
		t.Fatal(err)
	}
	v = joinChatGPT(t, s, session)
	if err := s.ResumeChatGPT(); err != nil {
		t.Fatal(err)
	}
	r = chatGPTCall(t, s, session, "chatgpt.publish", ChatGPTPublishRequest{ParticipationID: v.ParticipationID, OperationID: "after-stop", Text: "Continue group", RequestReplies: []chat.Participant{chat.Codex}})
	if r.OK || r.Error.Code != "run_stopped" {
		t.Fatal("renewal revived stopped work", r.Error)
	}
	report.ParticipationID = v.ParticipationID
	if r := chatGPTCall(t, s, session, "chatgpt.coordinator_report", report); r.OK {
		t.Fatal("late report revived run")
	}
	if _, err := s.ControlCoordination("resume"); err != nil {
		t.Fatal(err)
	}
	if r := chatGPTCall(t, s, session, "chatgpt.coordinator_report", report); r.OK {
		t.Fatal("old run ID reused after explicit resume")
	}
}
