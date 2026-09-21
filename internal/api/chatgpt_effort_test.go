//go:build !windows

package api

import (
	"fmt"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestChatGPTEffortValidationReceiptsAndRepetition(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil, failingChatGPTTestAgent{})
	view := joinChatGPT(t, s, session)
	if view.Moderator != chat.Codex || len(view.EffortCapabilities) != 2 || view.EffortCapabilities[0].CapabilitySource != "provider_validation" {
		t.Fatalf("join effort discovery: %+v", view.EffortCapabilities)
	}
	input := ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "invalid", Text: "Inspect this result", RequestReplies: []chat.Participant{chat.Codex}}
	input.Efforts = map[chat.Participant]string{chat.Claude: "low"}
	response := chatGPTCall(t, s, session, "chatgpt.publish", input)
	if response.OK || response.Error.Code != "invalid_effort" {
		t.Fatalf("unrelated effort accepted: %+v", response)
	}
	_, messages := o.Snapshot()
	state, _ := s.ChatGPTStatus()
	if len(messages) != 0 || state.ExchangesRemaining != 32 {
		t.Fatal("invalid effort posted or consumed budget")
	}
	for i, effort := range []string{"low", "medium", "high"} {
		input.OperationID = fmt.Sprintf("attempt-%d", i)
		input.Efforts = map[chat.Participant]string{chat.Codex: effort}
		input.EffortReason = "Brief inspection"
		response = chatGPTCall(t, s, session, "chatgpt.publish", input)
		if !response.OK {
			t.Fatal(response.Error)
		}
		receipt := response.Result.(map[string]any)
		if receipt["efforts"].(map[chat.Participant]string)[chat.Codex] != effort || receipt["effort_reason"] != input.EffortReason {
			t.Fatalf("accepted effort missing: %+v", receipt)
		}
		view = awaitChatGPTReplies(t, s, session, view.ParticipationID)
		found := false
		for _, reply := range view.ReplyResults {
			if reply.SourceSequence == receipt["sequence"] {
				found = reply.RequestedEffort == effort && reply.EffortReason == input.EffortReason && reply.EffortStatus.AppliedEffort == effort && reply.EffortStatus.ReportedEffort == ""
			}
		}
		if !found {
			t.Fatalf("failed reply lost effort or invented confirmation: %+v", view.ReplyResults)
		}
		duplicate := chatGPTCall(t, s, session, "chatgpt.publish", input)
		if !duplicate.OK || duplicate.Result.(map[string]any)["duplicate"] != true {
			t.Fatal("identical retry was not reused", duplicate.Error)
		}
		changed := input
		changed.EffortReason = "Different reason"
		if conflict := chatGPTCall(t, s, session, "chatgpt.publish", changed); conflict.OK {
			t.Fatal("changed reason reused operation id")
		}
	}
	input.OperationID = "attempt-3"
	input.Efforts = map[chat.Participant]string{chat.Codex: "max"}
	if response := chatGPTCall(t, s, session, "chatgpt.publish", input); response.OK || response.Error.Code != "no_progress" {
		t.Fatalf("effort change bypassed repetition guard: %+v", response)
	}
}

func TestChatGPTEffortRejectsTextOnlyAndInvalidWorkWithoutSideEffects(t *testing.T) {
	s, o, _, session := chatGPTService(t, nil)
	view := joinChatGPT(t, s, session)
	for _, call := range []struct {
		method string
		input  any
	}{
		{"chatgpt.publish", ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "text", Text: "Status only", Efforts: map[chat.Participant]string{chat.Codex: "low"}}},
		{"chatgpt.request_work", ChatGPTWorkRequest{ParticipationID: view.ParticipationID, OperationID: "work", Text: "Bounded task", Target: chat.Codex, Effort: "invented"}},
		{"chatgpt.request_round", ChatGPTRoundRequest{ParticipationID: view.ParticipationID, OperationID: "round", Text: "Review", Efforts: map[chat.Participant]string{chat.Claude: "low"}}},
	} {
		response := chatGPTCall(t, s, session, call.method, call.input)
		if response.OK || response.Error.Code != "invalid_effort" {
			t.Fatalf("%s: %+v", call.method, response)
		}
	}
	_, messages := o.Snapshot()
	state, _ := s.ChatGPTStatus()
	if len(messages) != 0 || state.ExchangesRemaining != 32 {
		t.Fatal("invalid effort had side effects")
	}
}
