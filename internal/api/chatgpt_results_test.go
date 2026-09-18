package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestChatGPTReplyResultReportsSafeCauseAndRetainedOutput(t *testing.T) {
	now := time.Now().UTC()
	job := chat.ConversationJob{ID: "reply", SourceSequence: 12, State: chat.ConversationCancelled,
		ReasonCode: chat.ReasonChatGPTLeft, CompletedAt: &now, HasPartialResponse: true,
		TerminalReason: "secret provider error at /private/token-file"}
	result := chatGPTReply(job, chat.Codex)
	if result.ReasonCode != chat.ReasonChatGPTLeft || result.CompletedAt == nil || !result.HasPartialResponse {
		t.Fatalf("incomplete result: %+v", result)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "/private") {
		t.Fatal("raw error leaked")
	}
	job.ReasonCode = chat.ConversationReason("secret injection")
	if chatGPTReply(job, chat.Codex).ReasonCode != chat.ReasonUnknown {
		t.Fatal("untrusted reason exposed")
	}
	job.ReasonCode, job.CompletedAt = "", nil
	legacy := chatGPTReply(job, chat.Codex)
	if legacy.ReasonCode != chat.ReasonUnknown || legacy.CompletedAt != nil {
		t.Fatal("legacy cause/time fabricated")
	}
	job.State, job.AnswerSequence = chat.ConversationAnswered, 42
	answer := chatGPTReply(job, chat.Codex)
	if answer.ReasonCode != "" || answer.AnswerSequence != 42 {
		t.Fatal("answer reference missing")
	}
}
