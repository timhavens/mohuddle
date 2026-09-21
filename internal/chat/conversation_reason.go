package chat

import "time"

// ConversationReason is an allowlisted public cause, never a provider error.
type ConversationReason string

const (
	ReasonHostStop           ConversationReason = "host_stopped"
	ReasonHostRestart        ConversationReason = "host_restarted"
	ReasonHostClosed         ConversationReason = "host_closed"
	ReasonChatGPTLeft        ConversationReason = "chatgpt_left"
	ReasonGrantRevoked       ConversationReason = "grant_revoked"
	ReasonGrantExpired       ConversationReason = "grant_expired"
	ReasonDeadline           ConversationReason = "deadline_exceeded"
	ReasonAccess             ConversationReason = "access_required"
	ReasonWriteDenied        ConversationReason = "write_not_allowed"
	ReasonInvalidAccess      ConversationReason = "invalid_access_request"
	ReasonEventQueueOverflow ConversationReason = "event_queue_overflow"
	ReasonEffortUnsupported  ConversationReason = "effort_unsupported"
	ReasonProvider           ConversationReason = "provider_error"
	ReasonPersistence        ConversationReason = "persistence_error"
	ReasonNoAnswer           ConversationReason = "no_answer"
	ReasonUnknown            ConversationReason = "unknown"
)

func (r ConversationReason) Safe() ConversationReason {
	switch r {
	case ReasonHostStop, ReasonHostRestart, ReasonHostClosed, ReasonChatGPTLeft, ReasonGrantRevoked, ReasonGrantExpired, ReasonDeadline, ReasonAccess, ReasonWriteDenied, ReasonInvalidAccess, ReasonEventQueueOverflow, ReasonEffortUnsupported, ReasonProvider, ReasonPersistence, ReasonNoAnswer:
		return r
	default:
		return ReasonUnknown
	}
}

func (r ConversationReason) Description() string {
	switch r.Safe() {
	case ReasonHostStop:
		return "Stopped by the host"
	case ReasonHostRestart:
		return "Interrupted by a room restart"
	case ReasonHostClosed:
		return "Room closed"
	case ReasonChatGPTLeft:
		return "ChatGPT explicitly left the room"
	case ReasonGrantRevoked:
		return "ChatGPT access was revoked or replaced"
	case ReasonGrantExpired:
		return "ChatGPT room access expired"
	case ReasonDeadline:
		return "Response deadline expired"
	case ReasonAccess:
		return "Requested access is outside the granted scope"
	case ReasonWriteDenied:
		return "A read-only task requested write access"
	case ReasonInvalidAccess:
		return "Responder repeatedly requested already-authorized read access"
	case ReasonEventQueueOverflow:
		return "MoHuddle could not keep up with the response stream"
	case ReasonEffortUnsupported:
		return "Requested effort is unsupported by the current model"
	case ReasonProvider:
		return "Provider failed"
	case ReasonPersistence:
		return "Could not save the reply"
	case ReasonNoAnswer:
		return "Provider returned no public answer"
	default:
		return "Cause was not recorded"
	}
}

func (j *ConversationJob) Finish(reason ConversationReason, at time.Time) {
	j.ReasonCode = reason
	at = at.UTC()
	j.CompletedAt = &at
}
