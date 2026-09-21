package api

import (
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type ChatGPTDraftRequest struct {
	ParticipationID string `json:"participation_id"`
	ReplyID         string `json:"reply_id" jsonschema:"ID of a terminal unsuccessful reply returned by mohuddle_read. Read its source message first."`
	DraftID         string `json:"draft_id,omitempty" jsonschema:"Retain the returned draft_id when paging. Omit to select the latest available attempt."`
	Segment         int    `json:"segment,omitempty"`
	Offset          int    `json:"offset,omitempty" jsonschema:"Character offset within the selected draft segment."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Characters per page: default 8000, maximum 16000."`
}

type ChatGPTDraft struct {
	ReplyID          string           `json:"reply_id"`
	DraftID          string           `json:"draft_id"`
	Participant      chat.Participant `json:"participant"`
	SourceSequence   uint64           `json:"source_sequence"`
	StartedAt        time.Time        `json:"started_at"`
	CompletedAt      time.Time        `json:"completed_at"`
	Incomplete       bool             `json:"incomplete"`
	CaptureTruncated *bool            `json:"capture_truncated"`
	Text             string           `json:"text"`
	Segment          int              `json:"segment"`
	SegmentCount     int              `json:"segment_count"`
	NextSegment      int              `json:"next_segment"`
	NextOffset       int              `json:"next_offset"`
	HasMore          bool             `json:"has_more"`
}

// Only explicit attempt links, or unambiguous legacy conversation/message
// links, establish ownership. Timestamps and participant names alone do not.
func replyDrafts(state chat.Room, messages []chat.Message, job chat.ConversationJob) []chat.TurnRecord {
	if !job.State.Terminal() || job.State == chat.ConversationAnswered || job.RemoteMessageID == "" {
		return nil
	}
	sourceOK := false
	for _, m := range messages {
		if m.Sequence == job.SourceSequence && m.Author == chat.ChatGPT {
			sourceOK = true
			break
		}
	}
	if !sourceOK {
		return nil
	}
	links := map[string]chat.Participant{}
	for _, a := range job.Attempts {
		if a.TurnID != "" {
			links[a.TurnID] = a.Participant
		}
	}
	for _, m := range messages {
		if m.ConversationID != job.ID || m.TurnID == "" || !m.Author.ValidAgent() {
			continue
		}
		ambiguous := false
		for _, other := range messages {
			if other.TurnID == m.TurnID && other.ConversationID != "" && other.ConversationID != job.ID {
				ambiguous = true
				break
			}
		}
		if !ambiguous {
			links[m.TurnID] = m.Author
		}
	}
	var result []chat.TurnRecord
	for i := len(state.TurnHistory) - 1; i >= 0; i-- {
		t := state.TurnHistory[i]
		if owner, ok := links[t.ID]; !ok || owner != t.Participant || t.State != chat.TurnRecordInterrupted || len(t.Drafts) == 0 {
			continue
		}
		t.Drafts = append([]string(nil), t.Drafts...)
		for j := range t.Drafts {
			t.Drafts[j] = agent.SanitizeResponseDraft(t.Drafts[j])
		}
		result = append(result, t)
	}
	return result
}

func (s *Service) readReplyDraftLocked(request Request) HandleResult {
	v, err := decodeChatGPTPayload[ChatGPTDraftRequest](request)
	if err != nil || v.ReplyID == "" || v.Segment < 0 || v.Offset < 0 || v.Limit < 0 || v.Limit > 16000 {
		return failed(request, "invalid_request", "select a reply and valid draft page (limit 1–16000)")
	}
	if !s.validParticipationLocked(v.ParticipationID) {
		return failed(request, "not_joined", "join this room before reading drafts")
	}
	state, messages := s.controller.Snapshot()
	for _, job := range state.Conversations {
		if job.ID != v.ReplyID {
			continue
		}
		if job.SourceSequence > s.chatgpt.delivered {
			return failed(request, "invalid_request", "read the source message before recovering its reply")
		}
		drafts := replyDrafts(state, messages, job)
		for _, draft := range drafts {
			if v.DraftID != "" && draft.ID != v.DraftID {
				continue
			}
			if v.Segment >= len(draft.Drafts) {
				return failed(request, "invalid_request", "draft segment is outside the retained response")
			}
			text := []rune(draft.Drafts[v.Segment])
			if v.Offset > len(text) {
				return failed(request, "invalid_request", "draft offset is outside the retained segment")
			}
			if v.Limit == 0 {
				v.Limit = 8000
			}
			end := min(len(text), v.Offset+v.Limit)
			nextSegment, nextOffset := v.Segment, end
			if end == len(text) {
				nextSegment++
				nextOffset = 0
			}
			var truncated *bool
			if draft.DraftCaptureVersion > 0 {
				value := draft.DraftTruncated
				truncated = &value
			}
			s.chatgpt.lease = time.Now().Add(chatGPTLease)
			return succeeded(request, ChatGPTDraft{ReplyID: job.ID, DraftID: draft.ID, Participant: draft.Participant, SourceSequence: job.SourceSequence, StartedAt: draft.StartedAt, CompletedAt: draft.CompletedAt, Incomplete: true, CaptureTruncated: truncated, Text: string(text[v.Offset:end]), Segment: v.Segment, SegmentCount: len(draft.Drafts), NextSegment: nextSegment, NextOffset: nextOffset, HasMore: nextSegment < len(draft.Drafts)})
		}
		break
	}
	return failed(request, "draft_unavailable", "no retained draft with a verified link is available for this failed reply; it may have expired from turn history")
}
