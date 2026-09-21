package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

const ChatGPTConnectionVersion = "mohuddle.chatgpt.v1"
const chatGPTLease = 2 * time.Minute

// ChatGPTConnection is consumed by the local stdio bridge. Never return this
// object to an MCP caller, print it, or store it in a room transcript.
type ChatGPTConnection struct {
	Version   string    `json:"version"`
	Socket    string    `json:"socket"`
	RoomID    string    `json:"room_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type chatGPTController interface {
	UpdateChatGPTState(chat.ChatGPTState)
	EndChatGPTParticipation(chat.ConversationReason)
	ResumeChatGPT()
	PublishChatGPT(string, uint64, []chat.Participant, chat.RouteMetadata, ...chat.EffortSelection) (chat.Message, bool, error)
	RequestChatGPTWork(string, chat.Participant, uint64, chat.RouteMetadata, ...chat.EffortSelection) (chat.Message, bool, error)
	RequestChatGPTRound(string, []chat.Participant, uint64, chat.RouteMetadata, ...chat.EffortSelection) (chat.Message, bool, error)
	EffortCapabilities() []chat.EffortCapability
}

type chatGPTAccess struct {
	socket, path, grant, roomID string
	hash                        [32]byte
	expires                     time.Time
	revoked                     chan struct{}
	participation, clientKey    string
	lease                       time.Time
	delivered, human            uint64
	exchanges                   int
	limits                      chat.ChatGPTLimits
	repeated                    map[[32]byte]int
	noProgress                  bool
	progressAfter               uint64
	progressText                map[[32]byte]struct{}
	completedWork               map[string]struct{}
	window                      time.Time
	posts                       int
	reading                     bool
	audit                       *AuditLog
}

// ConfigureChatGPT enables trusted TUI controls only. No grant is issued here.
func (s *Service) ConfigureChatGPT(socket, connectionPath string, audit *AuditLog) {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	s.chatgpt.socket, s.chatgpt.path, s.chatgpt.audit = socket, connectionPath, audit
}

func (s *Service) EnableChatGPT(ttl time.Duration) (string, error) {
	if ttl < time.Minute || ttl > 24*time.Hour {
		return "", fmt.Errorf("ChatGPT access duration must be between 1m and 24h")
	}
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	return s.enableChatGPTLocked(ttl)
}

// EnsureChatGPT is the idempotent local join operation. Recovering a tunnel must
// not rotate a live grant, steal a conversation's seat, unpause it, or replenish
// its work budget. EnableChatGPT remains the explicit credential rotation.
func (s *Service) EnsureChatGPT(ttl time.Duration) (string, error) {
	if ttl < time.Minute || ttl > 24*time.Hour {
		return "", fmt.Errorf("ChatGPT access duration must be between 1m and 24h")
	}
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	if s.chatgptEnabledLocked() {
		return s.chatgpt.path, nil
	}
	return s.enableChatGPTLocked(ttl)
}

func (s *Service) enableChatGPTLocked(ttl time.Duration) (string, error) {
	a := &s.chatgpt
	controller, ok := s.controller.(chatGPTController)
	if !ok || a.socket == "" || a.path == "" {
		return "", fmt.Errorf("ChatGPT requires the private local API socket")
	}
	state, _ := s.controller.Snapshot()
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	grant, err := NewID()
	if err != nil {
		return "", err
	}
	expires := time.Now().UTC().Add(ttl)
	connection := ChatGPTConnection{ChatGPTConnectionVersion, a.socket, state.ID, token, expires}
	if err := writeChatGPTConnection(a.path, connection); err != nil {
		return "", err
	}
	if a.revoked != nil {
		close(a.revoked)
	}
	controller.UpdateChatGPTState(chat.ChatGPTState{})
	*a = chatGPTAccess{socket: a.socket, path: a.path, audit: a.audit, roomID: state.ID,
		limits: a.effectiveLimits(), grant: grant, hash: sha256.Sum256([]byte(token)), expires: expires, revoked: make(chan struct{})}
	s.updateChatGPTStateLocked()
	_ = a.audit.Append(AuditRecord{Action: "chatgpt.enable", RoomID: state.ID, Allowed: true, Permission: "participant-settings"})
	return a.path, nil
}

func (s *Service) RevokeChatGPT() error {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	a := &s.chatgpt
	if a.revoked != nil {
		close(a.revoked)
	}
	*a = chatGPTAccess{socket: a.socket, path: a.path, audit: a.audit, limits: a.effectiveLimits()}
	if controller, ok := s.controller.(chatGPTController); ok {
		controller.UpdateChatGPTState(chat.ChatGPTState{})
	}
	_ = a.audit.Append(AuditRecord{Action: "chatgpt.revoke", Allowed: true})
	if a.path != "" {
		if err := os.Remove(a.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Service) ResumeChatGPT() error {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	if !s.chatgptEnabledLocked() {
		return fmt.Errorf("enable ChatGPT with /join @chatgpt first")
	}
	s.chatgpt.resumeBudget()
	s.controller.(chatGPTController).ResumeChatGPT()
	s.updateChatGPTStateLocked()
	_ = s.chatgpt.audit.Append(AuditRecord{Action: "chatgpt.resume", RoomID: s.chatgpt.roomID, Allowed: true})
	return nil
}

func (s *Service) ChatGPTStatus() (chat.ChatGPTState, string) {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	s.updateChatGPTStateLocked()
	state, _ := s.controller.Snapshot()
	if state.ChatGPT == nil {
		return chat.ChatGPTState{}, ""
	}
	return *state.ChatGPT, s.chatgpt.path
}

func (s *Service) chatgptEnabledLocked() bool {
	a := &s.chatgpt
	return a.grant != "" && time.Now().Before(a.expires)
}

func (s *Service) updateChatGPTStateLocked() {
	a := &s.chatgpt
	if controller, ok := s.controller.(chatGPTController); ok {
		state, messages := s.controller.Snapshot()
		a.observeProgress(state, messages)
		reason := a.requestPauseReason()
		if state.ChatGPT != nil && state.ChatGPT.Paused {
			reason = "host_paused"
		}
		controller.UpdateChatGPTState(chat.ChatGPTState{Enabled: s.chatgptEnabledLocked(),
			Connected: s.chatgptEnabledLocked() && a.participation != "" && time.Now().Before(a.lease),
			ExpiresAt: a.expires, LeaseUntil: a.lease, ExchangesRemaining: max(0, a.effectiveLimits().Exchanges-a.exchanges),
			Limits: a.effectiveLimits(), PauseReason: reason})
	}
}

func (s *Service) authenticateChatGPT(value HelloRequest) *Session {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	a := &s.chatgpt
	hash := sha256.Sum256([]byte(value.Token))
	if !s.chatgptEnabledLocked() || subtle.ConstantTimeCompare(hash[:], a.hash[:]) != 1 || !validIdentifier(value.ClientID) {
		return nil
	}
	return &Session{Identity: s.InstanceID() + "/chatgpt-" + a.grant + "/" + value.ClientID, InstanceID: s.InstanceID(),
		Credential: a.grant, Kind: ClientChatGPT, RoomID: a.roomID,
		Scopes: map[Scope]bool{ScopeObserve: true, ScopeParticipate: true}}
}

func (s *Service) validChatGPTSessionLocked(session *Session, request Request) bool {
	return session != nil && session.Kind == ClientChatGPT && s.chatgptEnabledLocked() &&
		session.Credential == s.chatgpt.grant && session.RoomID == s.chatgpt.roomID &&
		(request.RoomID == "" || request.RoomID == s.chatgpt.roomID)
}

type ChatGPTJoinRequest struct {
	ClientKey string `json:"client_key"`
}
type ChatGPTReadRequest struct {
	ParticipationID string `json:"participation_id"`
	After           uint64 `json:"after"`
	Limit           int    `json:"limit,omitempty"`
	WaitSeconds     int    `json:"wait_seconds,omitempty"`
}
type ChatGPTPublishRequest struct {
	Efforts         map[chat.Participant]string `json:"efforts,omitempty" jsonschema:"Optional effort per requested reply participant. Select supported levels from effort_capabilities; keys must be in request_replies. Does not change standing room settings. Omit for a text-only post."`
	EffortReason    string                      `json:"effort_reason,omitempty" jsonschema:"Optional brief shareable reason for the explicit effort choices, at most 512 UTF-8 bytes."`
	ParticipationID string                      `json:"participation_id"`
	OperationID     string                      `json:"operation_id"`
	Text            string                      `json:"text"`
	ReplyTo         uint64                      `json:"reply_to,omitempty"`
	RequestReplies  []chat.Participant          `json:"request_replies,omitempty" jsonschema:"Up to four distinct peers for independent read-only answers. They may run concurrently; this is not a sequential moderated round or a draft-then-review pipeline. Omit to post text only."`
}
type ChatGPTWorkRequest struct {
	Effort          string           `json:"effort,omitempty" jsonschema:"Optional effort for this target throughout this operation. Select a supported level from effort_capabilities. Omission preserves standing settings; auto means provider default."`
	EffortReason    string           `json:"effort_reason,omitempty" jsonschema:"Optional brief shareable reason for the explicit effort, at most 512 UTF-8 bytes."`
	ParticipationID string           `json:"participation_id"`
	OperationID     string           `json:"operation_id" jsonschema:"Unique work request identifier. Reuse only for an identical retry; a new ID schedules new work."`
	Target          chat.Participant `json:"target" jsonschema:"One present local AI participant, for example codex. The task uses that participant's existing permissions and the room's current mode."`
	Text            string           `json:"text" jsonschema:"The complete task the user asked you to delegate, including scope, constraints, and expected result. Only this text is shared with the room."`
	ReplyTo         uint64           `json:"reply_to,omitempty"`
}
type ChatGPTRoundRequest struct {
	Efforts         map[chat.Participant]string `json:"efforts,omitempty" jsonschema:"Optional effort keyed by selected participant, including the current moderator, who always speaks last. Each choice applies only to that participant in this round. Select supported levels from effort_capabilities."`
	EffortReason    string                      `json:"effort_reason,omitempty" jsonschema:"Optional brief shareable reason for the explicit effort choices, at most 512 UTF-8 bytes."`
	ParticipationID string                      `json:"participation_id"`
	OperationID     string                      `json:"operation_id" jsonschema:"Unique round identifier. Reuse only for an identical retry."`
	Text            string                      `json:"text" jsonschema:"One discussion or review of material that already exists. Include the exact proposal and constraints. Do not prefix /round or describe a script of future actions."`
	Participants    []chat.Participant          `json:"participants,omitempty" jsonschema:"Up to four distinct present local AI participants. Omit for the normal room participants. The host moderator always speaks last."`
	ReplyTo         uint64                      `json:"reply_to,omitempty" jsonschema:"Sequence of the actual draft or result already read, when requesting a review of it."`
}
type ChatGPTLeaveRequest struct {
	ParticipationID string `json:"participation_id"`
}

type ChatGPTMessage struct {
	Sequence   uint64           `json:"sequence"`
	Author     chat.Participant `json:"author"`
	Target     chat.Participant `json:"target,omitempty"`
	Text       string           `json:"text"`
	ReplyTo    uint64           `json:"reply_to,omitempty"`
	Truncated  bool             `json:"truncated,omitempty"`
	WorkflowID string           `json:"workflow_id,omitempty"`
}
type ChatGPTReply struct {
	RequestedEffort    string                  `json:"requested_effort,omitempty"`
	EffortReason       string                  `json:"effort_reason,omitempty"`
	EffortStatus       chat.EffortStatus       `json:"effort_status,omitzero"`
	DraftAvailable     bool                    `json:"draft_available"`
	ReasonCode         chat.ConversationReason `json:"reason_code,omitempty"`
	CompletedAt        *time.Time              `json:"completed_at,omitempty"`
	AnswerSequence     uint64                  `json:"answer_sequence,omitempty"`
	HasPartialResponse bool                    `json:"has_partial_response,omitempty"`
	ID                 string                  `json:"id"`
	SourceSequence     uint64                  `json:"source_sequence"`
	Participant        chat.Participant        `json:"participant"`
	State              chat.ConversationState  `json:"state"`
}
type ChatGPTWork struct {
	Efforts        map[chat.Participant]string            `json:"efforts,omitempty"`
	EffortReason   string                                 `json:"effort_reason,omitempty"`
	EffortStatus   map[chat.Participant]chat.EffortStatus `json:"effort_status,omitempty"`
	WorkflowID     string                                 `json:"workflow_id"`
	SourceSequence uint64                                 `json:"source_sequence"`
	Target         chat.Participant                       `json:"target"`
	State          chat.WorkflowState                     `json:"state"`
	Kind           string                                 `json:"kind"`
	Participants   []chat.Participant                     `json:"participants,omitempty"`
	Moderator      chat.Participant                       `json:"moderator,omitempty"`
}
type ChatGPTView struct {
	Moderator          chat.Participant        `json:"moderator,omitempty"`
	EffortCapabilities []chat.EffortCapability `json:"effort_capabilities"`
	RoomID             string                  `json:"room_id"`
	ParticipationID    string                  `json:"participation_id"`
	State              chat.ChatGPTState       `json:"state"`
	Participants       []chat.Participant      `json:"participants"`
	Messages           []ChatGPTMessage        `json:"messages"`
	Replies            []ChatGPTReply          `json:"replies"`
	ReplyResults       []ChatGPTReply          `json:"reply_results"`
	Work               []ChatGPTWork           `json:"work"`
	NextAfter          uint64                  `json:"next_after"`
	HasMore            bool                    `json:"has_more"`
	Usage              string                  `json:"usage,omitempty"`
}

func (s *Service) handleChatGPT(ctx context.Context, session *Session, request Request) HandleResult {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	if !s.validChatGPTSessionLocked(session, request) {
		return failed(request, "authentication_failed", "ChatGPT room access expired or was revoked")
	}
	a := &s.chatgpt
	switch request.Type {
	case "chatgpt.join":
		value, err := decodeChatGPTPayload[ChatGPTJoinRequest](request)
		if err != nil || !validIdentifier(value.ClientKey) {
			return failed(request, "invalid_request", "a valid client key is required")
		}
		if a.participation != "" && time.Now().Before(a.lease) && a.clientKey != value.ClientKey {
			return failed(request, "already_joined", "another ChatGPT conversation is participating; leave it or wait for its lease to expire")
		}
		if a.participation == "" || a.clientKey != value.ClientKey || !time.Now().Before(a.lease) {
			id, err := NewID()
			if err != nil {
				return failed(request, "internal_error", "could not create participation")
			}
			// Replace presence without cancelling work accepted under this grant.
			s.controller.(chatGPTController).UpdateChatGPTState(chat.ChatGPTState{Enabled: true, ExpiresAt: a.expires})
			a.participation, a.clientKey, a.delivered = id, value.ClientKey, 0
		}
		a.lease = time.Now().Add(chatGPTLease)
		s.updateChatGPTStateLocked()
		return succeeded(request, s.chatGPTViewLocked(0, 50))
	case "chatgpt.read_reply_draft":
		return s.readReplyDraftLocked(request)
	case "chatgpt.read":
		value, err := decodeChatGPTPayload[ChatGPTReadRequest](request)
		if err != nil || value.WaitSeconds < 0 || value.WaitSeconds > 25 || value.Limit < 0 || value.Limit > 100 {
			return failed(request, "invalid_request", "read supports limit 1–100 and wait_seconds 0–25")
		}
		if !s.validParticipationLocked(value.ParticipationID) {
			return failed(request, "not_joined", "join this room again; participation expired or ended")
		}
		if a.reading && value.WaitSeconds > 0 {
			return failed(request, "busy", "one room read is already waiting; reuse its result")
		}
		if value.After > a.delivered {
			return failed(request, "invalid_cursor", "cursor exceeds delivered room history")
		}
		if value.Limit == 0 {
			value.Limit = 50
		}
		a.lease = time.Now().Add(chatGPTLease)
		stream, stop := s.controller.SubscribeEvents(16)
		defer stop()
		timer := time.NewTimer(time.Duration(value.WaitSeconds) * time.Second)
		defer timer.Stop()
		for {
			view := s.chatGPTViewLocked(value.After, value.Limit)
			if len(view.Messages) > 0 || value.WaitSeconds == 0 || view.State.Paused {
				return succeeded(request, view)
			}
			revoked := a.revoked
			a.reading = true
			s.chatgptMu.Unlock()
			timedOut := false
			select {
			case <-ctx.Done():
				timedOut = true
			case <-timer.C:
				timedOut = true
			case <-revoked:
				timedOut = true
			case _, ok := <-stream:
				timedOut = !ok
			}
			s.chatgptMu.Lock()
			if a.grant == session.Credential {
				a.reading = false
			}
			if !s.validChatGPTSessionLocked(session, request) || !s.validParticipationLocked(value.ParticipationID) {
				return failed(request, "authentication_failed", "ChatGPT room access expired or ended")
			}
			if ctx.Err() != nil {
				return failed(request, "cancelled", "room read cancelled")
			}
			if timedOut {
				return succeeded(request, s.chatGPTViewLocked(value.After, value.Limit))
			}
		}
	case "chatgpt.publish", "chatgpt.request_work", "chatgpt.request_round":
		work := request.Type == "chatgpt.request_work"
		round := request.Type == "chatgpt.request_round"
		var value ChatGPTPublishRequest
		var target chat.Participant
		var participants []chat.Participant
		var err error
		if round {
			var task ChatGPTRoundRequest
			task, err = decodeChatGPTPayload[ChatGPTRoundRequest](request)
			for _, participant := range task.Participants {
				participants = append(participants, chat.Participant(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(participant)), "@"))))
			}
			value = ChatGPTPublishRequest{ParticipationID: task.ParticipationID, OperationID: task.OperationID, Text: task.Text, ReplyTo: task.ReplyTo, Efforts: task.Efforts, EffortReason: task.EffortReason}
		} else if work {
			var task ChatGPTWorkRequest
			task, err = decodeChatGPTPayload[ChatGPTWorkRequest](request)
			target = chat.Participant(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(task.Target)), "@")))
			if !target.ValidAgent() {
				return failed(request, "invalid_request", "target must name one present local AI participant")
			}
			value = ChatGPTPublishRequest{ParticipationID: task.ParticipationID, OperationID: task.OperationID, Text: task.Text, ReplyTo: task.ReplyTo, EffortReason: task.EffortReason}
			if task.Effort != "" {
				value.Efforts = map[chat.Participant]string{target: task.Effort}
			}
		} else {
			value, err = decodeChatGPTPayload[ChatGPTPublishRequest](request)
		}
		if err != nil || !validIdentifier(value.OperationID) || len(value.Text) > 16000 || strings.TrimSpace(value.Text) == "" {
			return failed(request, "invalid_request", "a unique operation_id and 1–16000 bytes of text are required")
		}
		if !s.validParticipationLocked(value.ParticipationID) {
			return failed(request, "not_joined", "join this room before contributing")
		}
		if value.ReplyTo > a.delivered {
			return failed(request, "invalid_request", "read the reply target before replying")
		}
		state, messages := s.controller.Snapshot()
		if state.ChatGPT == nil || state.ChatGPT.Paused {
			return failed(request, "paused", "the host paused ChatGPT; use /chatgpt resume in MoHuddle")
		}
		if correction := chatGPTComposerCommand(value.Text); correction != "" {
			return failed(request, "composer_command_not_supported", correction)
		}
		a.observeProgress(state, messages)
		exchange := work || round || len(value.RequestReplies) > 0
		targets := value.RequestReplies
		if work {
			targets = []chat.Participant{target}
		} else if round {
			targets = participants
		}
		fingerprint := chatGPTRequestFingerprint(request.Type, value.Text, targets)
		operation := sha256.Sum256([]byte(a.grant + "\x00" + value.OperationID))
		route := chat.RouteMetadata{MessageID: fmt.Sprintf("%x", operation), OriginInstanceID: s.InstanceID(), OriginClientID: session.Identity}
		duplicate := false
		for _, message := range messages {
			if message.Route != nil && message.Route.MessageID == route.MessageID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if exchange && a.exchanges >= a.effectiveLimits().Exchanges {
				return s.chatGPTBudgetFailure(request, "exchange_limit", "room exchange budget reached; accepted work continues. Use /chatgpt resume or /chatgpt limits in MoHuddle")
			}
			if exchange && (a.noProgress || a.repeated[fingerprint] >= a.effectiveLimits().RepeatedRequests) {
				a.noProgress = true
				s.updateChatGPTStateLocked()
				return s.chatGPTBudgetFailure(request, "no_progress", "repeated identical requests without new peer content or completed work; accepted work continues. Inspect results, then use /chatgpt resume in MoHuddle")
			}
			if time.Since(a.window) >= time.Minute {
				a.window, a.posts = time.Now(), 0
			}
			if a.posts >= 20 {
				return failed(request, "rate_limited", "room posting limit reached; wait a minute")
			}
		}
		var message chat.Message
		var created bool
		effort := chat.EffortSelection{Efforts: value.Efforts, Reason: value.EffortReason}
		if round {
			message, created, err = s.controller.(chatGPTController).RequestChatGPTRound(value.Text, participants, value.ReplyTo, route, effort)
		} else if work {
			message, created, err = s.controller.(chatGPTController).RequestChatGPTWork(value.Text, target, value.ReplyTo, route, effort)
		} else {
			message, created, err = s.controller.(chatGPTController).PublishChatGPT(value.Text, value.ReplyTo, value.RequestReplies, route, effort)
		}
		if created {
			a.posts++
			if exchange {
				a.exchanges++
				if a.repeated == nil {
					a.repeated = make(map[[32]byte]int)
				}
				a.repeated[fingerprint]++
			}
		}
		s.updateChatGPTStateLocked()
		scheduled := []chat.Participant{}
		if err == nil {
			if round && message.Round != nil {
				scheduled = append(scheduled, message.Round.Participants...)
			} else if work {
				scheduled = append(scheduled, target)
			} else {
				scheduled = append(scheduled, value.RequestReplies...)
			}
		}
		result := map[string]any{
			"efforts": message.EffortSelection.Efforts, "effort_reason": message.EffortSelection.Reason,
			"sequence": message.Sequence, "duplicate": !created && message.Sequence != 0,
			"message_posted": message.Sequence != 0, "agent_scheduled": len(scheduled) != 0,
			"scheduled_agents": scheduled, "exchanges_remaining": max(0, a.effectiveLimits().Exchanges-a.exchanges),
			"limits": a.effectiveLimits(), "pause_reason": a.requestPauseReason(),
		}
		action, next := "post", "No agent was requested. This message does not schedule future steps."
		switch {
		case round:
			action, next = "round", "Use mohuddle_read until this workflow finishes; inspect the actual reviews and moderator synthesis. Completion does not establish consensus or authorize implementation."
		case work:
			action, next = "work", "Use mohuddle_read until this workflow finishes and read its actual result before scheduling any dependent review or work."
		case len(value.RequestReplies) > 0:
			action, next = "replies", "Use mohuddle_read to collect replies and reply_results for this source sequence. These are independent read-only replies, not a moderated round or a pipeline."
		}
		result["action"], result["next_action"] = action, next
		if (work || round) && message.WorkflowID != "" {
			state, _ := s.controller.Snapshot()
			result["workflow_id"], result["work_state"] = message.WorkflowID, state.Workflows[message.WorkflowID].State
			if message.Round != nil {
				result["moderator"] = message.Round.Moderator
			}
		}
		if err != nil {
			code := "publish_failed"
			if work {
				code = "work_failed"
			} else if round {
				code = "round_failed"
			}
			var effortErr *chat.EffortError
			if errors.As(err, &effortErr) {
				code = "invalid_effort"
			}
			result["failure_code"], result["failure_reason"] = code, err.Error()
			failure := failed(request, code, err.Error())
			failure.Response.Result = result
			return failure
		}
		if work {
			_ = a.audit.Append(AuditRecord{Action: "chatgpt.request_work", RoomID: a.roomID, Allowed: true, Permission: "participant-settings"})
		} else if round {
			_ = a.audit.Append(AuditRecord{Action: "chatgpt.request_round", RoomID: a.roomID, Allowed: true, Permission: "read-only"})
		}
		return succeeded(request, result)
	case "chatgpt.leave":
		value, err := decodeChatGPTPayload[ChatGPTLeaveRequest](request)
		// An explicit leave still withdraws accepted work after presence has
		// expired. A superseded participation ID cannot end a newer join.
		if err != nil || value.ParticipationID == "" || subtle.ConstantTimeCompare([]byte(value.ParticipationID), []byte(a.participation)) != 1 {
			return failed(request, "not_joined", "participation already ended or expired")
		}
		a.participation, a.clientKey, a.lease = "", "", time.Time{}
		s.controller.(chatGPTController).EndChatGPTParticipation(chat.ReasonChatGPTLeft)
		s.updateChatGPTStateLocked()
		return succeeded(request, map[string]bool{"left": true})
	default:
		return failed(request, "forbidden", "ChatGPT can only join, read, publish, request work or a read-only round, and leave its granted room")
	}
}

// Refuse the common accidental command-as-message form without interpreting or
// executing any text. Quoted commands and commands mentioned within prose remain
// ordinary content. The model must choose a structured scheduling operation.
func chatGPTComposerCommand(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	switch strings.ToLower(words[0]) {
	case "/round":
		return "/round is a composer command, not plugin text. Nothing was posted or scheduled. Use mohuddle_request_round with one existing proposal in text; wait for its result with mohuddle_read."
	case "/ask":
		return "/ask is a composer command, not plugin text. Nothing was posted or scheduled. Use mohuddle_publish with request_replies naming the participants, then mohuddle_read."
	case "/delegate":
		return "/delegate is a composer command, not plugin text. Nothing was posted or scheduled. Use mohuddle_request_work for one authorized task, then mohuddle_read before starting a dependent step."
	}
	return ""
}

func decodeChatGPTPayload[T any](request Request) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(request.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return value, fmt.Errorf("unexpected trailing data")
	}
	return value, nil
}

func (s *Service) validParticipationLocked(id string) bool {
	a := &s.chatgpt
	return id != "" && subtle.ConstantTimeCompare([]byte(id), []byte(a.participation)) == 1 && time.Now().Before(a.lease)
}

func chatGPTReply(job chat.ConversationJob, participant chat.Participant) ChatGPTReply {
	result := ChatGPTReply{ID: job.ID, SourceSequence: job.SourceSequence, Participant: participant, State: job.State,
		RequestedEffort: job.EffortSelection.Efforts[participant], EffortReason: job.EffortSelection.Reason,
		CompletedAt: job.CompletedAt, AnswerSequence: job.AnswerSequence, HasPartialResponse: job.HasPartialResponse}
	for i := len(job.Attempts) - 1; i >= 0; i-- {
		if job.Attempts[i].Participant == participant {
			result.EffortStatus = job.Attempts[i].Effort
			break
		}
	}
	if job.State.Terminal() && job.State != chat.ConversationAnswered {
		result.ReasonCode = job.ReasonCode.Safe()
		if job.TerminalReason == "codex event queue overflow" {
			result.ReasonCode = chat.ReasonEventQueueOverflow
		}
	}
	return result
}

func (s *Service) chatGPTViewLocked(after uint64, limit int) ChatGPTView {
	s.updateChatGPTStateLocked()
	state, messages := s.controller.Snapshot()
	// Older rooms retain interrupted public drafts in turn history but do not
	// have HasPartialResponse on the reply. Derive only this safe availability
	// bit from recorded turn/message links, without exporting draft contents.
	partialTurns, partialReplies := map[string]bool{}, map[string]bool{}
	for _, turn := range state.TurnHistory {
		if turn.State == chat.TurnRecordInterrupted && len(turn.Drafts) > 0 {
			partialTurns[turn.ID] = true
		}
	}
	for _, message := range messages {
		if partialTurns[message.TurnID] && message.ConversationID != "" {
			partialReplies[message.ConversationID] = true
		}
	}
	for i := range state.Conversations {
		if partialReplies[state.Conversations[i].ID] {
			state.Conversations[i].HasPartialResponse = true
		}
	}
	view := ChatGPTView{RoomID: state.ID, ParticipationID: s.chatgpt.participation,
		Moderator: state.Moderator, EffortCapabilities: s.controller.(chatGPTController).EffortCapabilities(),
		Participants: state.PresentAgents(), Messages: []ChatGPTMessage{}, Replies: []ChatGPTReply{}, ReplyResults: []ChatGPTReply{}, Work: []ChatGPTWork{}, NextAfter: after}
	if state.ChatGPT != nil {
		view.State = *state.ChatGPT
	}
	bytes := 0
	for _, message := range messages {
		if message.Sequence <= after {
			continue
		}
		if message.Kind != chat.MessageText || (message.Author != chat.User && message.Author != chat.ChatGPT && !message.Author.ValidAgent()) {
			view.NextAfter = message.Sequence
			continue
		}
		if len(view.Messages) >= limit || bytes >= 64000 {
			view.HasMore = true
			break
		}
		text := message.Text
		truncated := len(text) > 16000
		if truncated {
			text = strings.ToValidUTF8(text[:16000], "")
		}
		view.Messages = append(view.Messages, ChatGPTMessage{message.Sequence, message.Author, message.Target, text, message.ReplyTo, truncated, message.WorkflowID})
		bytes += len(text)
		view.NextAfter = message.Sequence
	}
	for _, job := range state.Conversations {
		if len(view.Replies) >= 20 {
			break
		}
		if !job.State.Terminal() && job.RemoteMessageID != "" {
			for _, message := range messages {
				if message.Sequence == job.SourceSequence && message.Author == chat.ChatGPT {
					participant := job.Assigned
					if participant == "" && len(job.Requested) > 0 {
						participant = job.Requested[0]
					}
					view.Replies = append(view.Replies, chatGPTReply(job, participant))
				}
			}
		}
	}
	// Preserve terminal reply states as well as pending replies. Disappearance
	// from the pending list alone does not prove that a participant answered.
	for i := len(state.Conversations) - 1; i >= 0 && len(view.ReplyResults) < 20; i-- {
		job := state.Conversations[i]
		if !job.State.Terminal() || job.RemoteMessageID == "" {
			continue
		}
		for _, message := range messages {
			if message.Sequence == job.SourceSequence && message.Author == chat.ChatGPT {
				participant := job.Assigned
				if participant == "" && len(job.Requested) > 0 {
					participant = job.Requested[0]
				}
				reply := chatGPTReply(job, participant)
				reply.DraftAvailable = len(replyDrafts(state, messages, job)) > 0
				view.ReplyResults = append(view.ReplyResults, reply)
				break
			}
		}
	}
	// Return bounded, newest-first status without internal errors, paths, tool
	// logs, or provider session identifiers. Terminal work remains observable.
	for i := len(messages) - 1; i >= 0 && len(view.Work) < 20; i-- {
		message := messages[i]
		if message.Author == chat.ChatGPT && message.IsWorkflowSource() {
			if record, ok := state.Workflows[message.WorkflowID]; ok {
				status := ChatGPTWork{WorkflowID: record.ID, SourceSequence: message.Sequence, Target: record.Target, State: record.State, Kind: "work",
					Efforts: record.EffortSelection.Efforts, EffortReason: record.EffortSelection.Reason, EffortStatus: record.EffortStatus}
				if message.Round != nil {
					status.Kind, status.Moderator = "round", message.Round.Moderator
					status.Participants = append([]chat.Participant(nil), message.Round.Participants...)
				}
				view.Work = append(view.Work, status)
			}
		}
	}
	s.chatgpt.delivered = max(s.chatgpt.delivered, view.NextAfter)
	return view
}

func writeChatGPTConnection(path string, connection ChatGPTConnection) error {
	if err := privateParent(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("connection path must be a regular private file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".chatgpt-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(connection); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func privateParent(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("an absolute private connection path is required")
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("connection parent must be a private directory (0700)")
	}
	return nil
}

func ReadChatGPTConnection(path string) (ChatGPTConnection, error) {
	var value ChatGPTConnection
	if err := privateParent(path); err != nil {
		return value, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return value, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 8192 {
		return value, fmt.Errorf("connection must be a private regular file (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return value, fmt.Errorf("connection changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return ChatGPTConnection{}, fmt.Errorf("invalid ChatGPT connection file")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ChatGPTConnection{}, fmt.Errorf("invalid ChatGPT connection file")
	}
	_, tokenError := hex.DecodeString(value.Token)
	if value.Version != ChatGPTConnectionVersion || !filepath.IsAbs(value.Socket) || !validIdentifier(value.RoomID) || len(value.Token) != 64 || tokenError != nil || !time.Now().Before(value.ExpiresAt) {
		return ChatGPTConnection{}, fmt.Errorf("ChatGPT connection is invalid or expired; enable it again in MoHuddle")
	}
	if err := privateParent(value.Socket); err != nil {
		return ChatGPTConnection{}, err
	}
	socket, err := os.Lstat(value.Socket)
	if err != nil || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm()&0o077 != 0 {
		return ChatGPTConnection{}, fmt.Errorf("ChatGPT requires a private active Unix socket")
	}
	return value, nil
}
