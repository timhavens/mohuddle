package api

import (
	"crypto/sha256"
	"fmt"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type ChatGPTMessageRequest struct {
	ParticipationID string `json:"participation_id"`
	Sequence        uint64 `json:"sequence" jsonschema:"Sequence of a public text message already delivered by mohuddle_read."`
	SHA256          string `json:"sha256,omitempty" jsonschema:"Retain the returned hash for subsequent pages; mismatches are rejected."`
	Offset          int    `json:"offset,omitempty" jsonschema:"Unicode character offset, using next_offset from the preceding page."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Characters per page, default 4000, maximum 8000."`
}

type ChatGPTMessagePage struct {
	Sequence        uint64           `json:"sequence"`
	Author          chat.Participant `json:"author"`
	SHA256          string           `json:"sha256"`
	Text            string           `json:"text"`
	NextOffset      int              `json:"next_offset"`
	TotalCharacters int              `json:"total_characters"`
	HasMore         bool             `json:"has_more"`
	Complete        bool             `json:"complete"`
	Truncated       bool             `json:"truncated"`
}

func (s *Service) readChatGPTMessageLocked(request Request) HandleResult {
	v, err := decodeChatGPTPayload[ChatGPTMessageRequest](request)
	if err != nil || v.Sequence == 0 || v.Offset < 0 || v.Limit < 0 || v.Limit > 8000 {
		return failed(request, "invalid_request", "select a message and valid page (1–8000 characters)")
	}
	if !s.validParticipationLocked(v.ParticipationID) {
		return failed(request, "not_joined", "join this room before reading messages")
	}
	if v.Sequence > s.chatgpt.delivered {
		return failed(request, "invalid_request", "read the message summary before paging its text")
	}
	if v.Offset > 0 && v.SHA256 == "" {
		return failed(request, "invalid_request", "subsequent pages require the returned sha256")
	}
	_, messages := s.controller.Snapshot()
	for _, m := range messages {
		if m.Sequence != v.Sequence {
			continue
		}
		if m.Kind != chat.MessageText || (m.Author != chat.User && m.Author != chat.ChatGPT && !m.Author.ValidAgent()) {
			return failed(request, "invalid_request", "only shared public text messages can be paged")
		}
		text := agent.SanitizeResponseDraft(m.Text)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		if v.SHA256 != "" && v.SHA256 != hash {
			return failed(request, "version_mismatch", "message version changed; restart from offset zero")
		}
		runes := []rune(text)
		if v.Offset > len(runes) {
			return failed(request, "invalid_request", "offset exceeds message length")
		}
		if v.Limit == 0 {
			v.Limit = 4000
		}
		end := min(len(runes), v.Offset+v.Limit)
		return succeeded(request, ChatGPTMessagePage{Sequence: m.Sequence, Author: m.Author, SHA256: hash, Text: string(runes[v.Offset:end]), NextOffset: end, TotalCharacters: len(runes), HasMore: end < len(runes), Complete: end == len(runes), Truncated: false})
	}
	return failed(request, "unavailable", "message is not in retained room history; no research was rerun")
}
