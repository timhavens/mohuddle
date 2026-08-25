package room

import (
	"fmt"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

type ParticipantStatus struct {
	Participant  chat.Participant    `json:"participant"`
	State        chat.SchedulerState `json:"state"`
	Action       string              `json:"action,omitempty"`
	WaitReason   string              `json:"wait_reason,omitempty"`
	Dependency   string              `json:"dependency,omitempty"`
	Elapsed      time.Duration       `json:"elapsed_ns"`
	LastActivity time.Duration       `json:"last_activity_ns"`
	Eligible     bool                `json:"eligible"`
	AwayReason   string              `json:"away_reason,omitempty"`
}

type StatusSnapshot struct {
	At                   time.Time                                         `json:"at"`
	WorkflowActive       bool                                              `json:"workflow_active"`
	WorkflowStage        string                                            `json:"workflow_stage"`
	Participants         []ParticipantStatus                               `json:"participants"`
	QueuedHumanInputs    int                                               `json:"queued_human_inputs"`
	PendingRouting       int                                               `json:"pending_routing"`
	QueuedConversations  int                                               `json:"queued_conversations"`
	UnreadReplies        int                                               `json:"unread_replies"`
	NeedsAttention       int                                               `json:"needs_attention"`
	ParticipantAttention int                                               `json:"participant_needs_attention"`
	Availability         map[chat.Participant]chat.ParticipantAvailability `json:"availability,omitempty"`
	ManualHolds          map[chat.Participant]chat.ManualProviderHold      `json:"manual_holds,omitempty"`
	Corrections          chat.CorrectionCounts                             `json:"corrections"`
	CorrectionsByAgent   map[chat.Participant]chat.CorrectionCounts        `json:"corrections_by_agent"`
	PendingRoster        []chat.ScheduledRosterAction                      `json:"pending_roster,omitempty"`
}

func (o *Orchestrator) StatusSnapshot() StatusSnapshot {
	now := time.Now().UTC()
	o.mu.Lock()
	defer o.mu.Unlock()
	result := StatusSnapshot{
		At: now, WorkflowActive: o.activeWork > 0, QueuedHumanInputs: len(o.room.PendingInputs), PendingRouting: len(o.room.PendingRoutes),
		Availability: cloneAvailability(o.room.Availability), ManualHolds: cloneMap(o.room.ManualProviderHolds),
	}
	for provider, availability := range result.Availability {
		if availability.RetryAt != nil && !now.Before(*availability.RetryAt) {
			delete(result.Availability, provider)
		}
	}
	result.Corrections, result.CorrectionsByAgent = chat.CorrectionStatistics(o.messages)
	for _, action := range o.room.RosterActions {
		if action.Status == chat.RosterActionPending {
			result.PendingRoster = append(result.PendingRoster, action)
		}
	}
	result.WorkflowStage = "idle"
	stageRank := 0
	participants := o.settingsParticipantsLocked()
	for _, participant := range participants {
		activity := o.room.Activities[participant]
		state := activity.State
		if !state.Valid() {
			state = chat.SchedulerIdle
		}
		last := time.Duration(0)
		if !activity.LastUpdateAt.IsZero() {
			last = now.Sub(activity.LastUpdateAt)
			if last < 0 {
				last = 0
			}
		}
		if state == chat.SchedulerActive && last >= 90*time.Second {
			state = chat.SchedulerQuiet
		}
		elapsed := time.Duration(0)
		if !activity.StartedAt.IsZero() {
			elapsed = now.Sub(activity.StartedAt)
			if elapsed < 0 {
				elapsed = 0
			}
		}
		eligibility := o.providerEligibilityLocked(participant, now, eligibilityOptions{ignoreSaturation: true})
		waitReason := activity.WaitReason
		if waitReason == "" && (state == chat.SchedulerQueued || state == chat.SchedulerWaiting || state == chat.SchedulerNeedsAttention) {
			waitReason = activity.Dependency
		}
		result.Participants = append(result.Participants, ParticipantStatus{
			Participant: participant, State: state, Action: activity.Action, WaitReason: waitReason,
			Dependency: activity.Dependency, Elapsed: elapsed, LastActivity: last,
			Eligible: eligibility.Eligible, AwayReason: eligibility.Reason,
		})
		if result.WorkflowActive && activity.Role != "" {
			rank := 0
			switch state {
			case chat.SchedulerActive, chat.SchedulerQuiet:
				rank = 3
			case chat.SchedulerWaiting:
				rank = 2
			case chat.SchedulerQueued:
				rank = 1
			}
			if rank > stageRank {
				stageRank = rank
				result.WorkflowStage = activity.Role
			}
		}
	}
	for _, conversation := range o.room.Conversations {
		switch conversation.DerivedInboxCategory() {
		case chat.ConversationInboxWorking:
			result.QueuedConversations++
		case chat.ConversationInboxActionNeeded:
			result.NeedsAttention++
		case chat.ConversationInboxNewAnswer:
			result.UnreadReplies++
		}
	}
	for _, activity := range o.room.Activities {
		if activity.State == chat.SchedulerNeedsAttention {
			result.ParticipantAttention++
		}
	}
	return result
}

func FormatStatusSnapshot(snapshot StatusSnapshot) string {
	workflow := "idle"
	if snapshot.WorkflowActive {
		workflow = "active · " + snapshot.WorkflowStage
	}
	lines := []string{
		"workflow: " + workflow,
		fmt.Sprintf("queues: %d human work input(s), %d routing decision(s), %d conversation(s)", snapshot.QueuedHumanInputs, snapshot.PendingRouting, snapshot.QueuedConversations),
		fmt.Sprintf("replies: %d unread, %d need attention", snapshot.UnreadReplies, snapshot.NeedsAttention),
		fmt.Sprintf("participants: %d need attention", snapshot.ParticipantAttention),
	}
	lines = append(lines, fmt.Sprintf("corrections: offered %d; accepted %d; retracted %d; pending %d", snapshot.Corrections.Offered, snapshot.Corrections.Accepted, snapshot.Corrections.Retracted, snapshot.Corrections.Pending))
	for _, participant := range chat.OrderedParticipants(mapParticipantKeys(snapshot.CorrectionsByAgent)) {
		counts := snapshot.CorrectionsByAgent[participant]
		lines = append(lines, fmt.Sprintf("corrections @%s: offered %d; accepted %d; retracted %d; pending %d; accepted received %d", participant, counts.Offered, counts.Accepted, counts.Retracted, counts.Pending, counts.AcceptedReceived))
	}
	for _, participant := range snapshot.Participants {
		line := fmt.Sprintf("@%s: %s", participant.Participant, strings.ReplaceAll(string(participant.State), "_", " "))
		if participant.Action != "" {
			line += " · " + participant.Action
		}
		if participant.WaitReason != "" {
			line += " · " + participant.WaitReason
		}
		if !participant.Eligible && participant.AwayReason != "" {
			line += " · unavailable: " + participant.AwayReason
		}
		if participant.Elapsed > 0 {
			line += " · elapsed " + formatStatusDuration(participant.Elapsed)
		}
		if participant.LastActivity > 0 {
			line += " · last activity " + formatStatusDuration(participant.LastActivity) + " ago"
		}
		lines = append(lines, line)
	}
	for _, provider := range chat.OrderedParticipants(mapParticipantKeys(snapshot.ManualHolds)) {
		hold := snapshot.ManualHolds[provider]
		lines = append(lines, fmt.Sprintf("@%s provider hold: %s", provider, hold.Reason))
	}
	for _, provider := range chat.OrderedParticipants(mapParticipantKeys(snapshot.Availability)) {
		availability := snapshot.Availability[provider]
		line := fmt.Sprintf("@%s provider restriction: %s", provider, availability.Reason)
		if availability.RetryAt != nil {
			line += " · retry " + availability.RetryAt.Local().Format(time.RFC3339)
		}
		lines = append(lines, line)
	}
	for _, action := range snapshot.PendingRoster {
		line := fmt.Sprintf("scheduled roster: %s @%s at %s · id %s · authorized by %s", action.Action, action.Participant, action.ExecuteAt.Local().Format(time.RFC3339), action.ID, action.AuthorizedBy)
		if action.Reason != "" {
			line += " · " + action.Reason
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func mapParticipantKeys[V any](values map[chat.Participant]V) []chat.Participant {
	result := make([]chat.Participant, 0, len(values))
	for participant := range values {
		result = append(result, participant)
	}
	return result
}

func (o *Orchestrator) answerOperationalStatus(text string, route *chat.RouteMetadata) error {
	id, err := store.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	o.mu.Lock()
	mode := o.room.WorkflowMode.WithDefault()
	source, err := o.appendRoutedUserMessageLocked("", text, nil, route, mode, chat.InputConversation, chat.IntentHigh, id)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	answer := FormatStatusSnapshot(o.StatusSnapshot())
	o.mu.Lock()
	message, err := o.appendConversationMessageLocked(chat.System, chat.MessageText, id, answer)
	if err == nil {
		job := chat.ConversationJob{
			ID: id, SourceSequence: source.Sequence, State: chat.ConversationAnswered, Class: chat.ConversationQuick,
			WorkflowMode: mode, AnswerSequence: message.Sequence, Unread: true,
			CreatedAt: now, UpdatedAt: now, StartedAt: &now, LastActivityAt: now,
		}
		o.room.Conversations = append(o.room.Conversations, job)
	}
	o.mu.Unlock()
	if err != nil {
		return err
	}
	o.send(Event{Type: EventMessage, Message: &source})
	o.send(Event{Type: EventMessage, Message: &message})
	jobs := o.ConversationJobs()
	if len(jobs) > 0 {
		job := jobs[len(jobs)-1]
		o.send(Event{Type: EventConversation, Conversation: &job})
	}
	return o.saveRoom()
}
