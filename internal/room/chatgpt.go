package room

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

// UpdateChatGPTState is called only by the trusted local access manager.
// Losing the grant also cancels its outstanding read-only peer conversations.
func (o *Orchestrator) UpdateChatGPTState(state chat.ChatGPTState) {
	o.mu.Lock()
	if previous := o.room.ChatGPT; previous != nil && previous.Enabled && state.Enabled {
		state.Paused = previous.Paused
	}
	if o.room.ChatGPT != nil && *o.room.ChatGPT == state {
		o.mu.Unlock()
		return
	}
	o.room.ChatGPT = &state
	var cancelled []chat.ConversationJob
	continuationCancelled := false
	if !state.Enabled {
		continuationCancelled = o.cancelContinuationsLocked("Original access ended; continuation will not resume on renewal")
		reason := chat.ReasonGrantRevoked
		if !state.ExpiresAt.IsZero() && !time.Now().Before(state.ExpiresAt) {
			reason = chat.ReasonGrantExpired
		}
		cancelled = o.cancelChatGPTConversationsLocked(reason)
	}
	o.mu.Unlock()
	if len(cancelled) > 0 || continuationCancelled {
		if err := o.saveRoom(); err != nil {
			o.send(Event{Type: EventError, Err: fmt.Errorf("save ended ChatGPT participation: %w", err)})
		}
		for i := range cancelled {
			o.send(Event{Type: EventConversation, Conversation: &cancelled[i]})
		}
	}
	o.send(Event{Type: EventQueueChanged})
}

func (o *Orchestrator) ResumeChatGPT() {
	o.mu.Lock()
	if o.room.ChatGPT != nil {
		o.room.ChatGPT.Paused = false
	}
	o.mu.Unlock()
	o.send(Event{Type: EventQueueChanged})
}

// EndChatGPTParticipation is reserved for explicit leave/revocation. A passive
// presence/lease update cannot withdraw authority for an accepted reply.
func (o *Orchestrator) EndChatGPTParticipation(reason chat.ConversationReason) {
	o.mu.Lock()
	if o.room.ChatGPT != nil {
		o.room.ChatGPT.Connected = false
		o.room.ChatGPT.LeaseUntil = time.Time{}
	}
	cancelled := o.cancelChatGPTConversationsLocked(reason)
	continuationCancelled := o.cancelContinuationsLocked("ChatGPT explicitly left")
	o.mu.Unlock()
	if len(cancelled) > 0 || continuationCancelled {
		if err := o.saveRoom(); err != nil {
			o.send(Event{Type: EventError, Err: fmt.Errorf("save ended ChatGPT replies: %w", err)})
		}
		for i := range cancelled {
			o.send(Event{Type: EventConversation, Conversation: &cancelled[i]})
		}
	}
	o.send(Event{Type: EventQueueChanged})
}

func (o *Orchestrator) cancelChatGPTConversationsLocked(reason chat.ConversationReason) []chat.ConversationJob {
	now := time.Now().UTC()
	var cancelled []chat.ConversationJob
	for i := range o.room.Conversations {
		job := &o.room.Conversations[i]
		if job.State.Terminal() || !o.chatGPTConversationLocked(job) {
			continue
		}
		if turn := o.activeTurns[job.Assigned]; turn.conversationID == job.ID && turn.cancel != nil {
			turn.cancel()
		}
		job.State, job.TerminalReason, job.UpdatedAt = chat.ConversationCancelled, reason.Description(), now
		job.Finish(reason, now)
		cancelled = append(cancelled, cloneConversationJobs([]chat.ConversationJob{*job})[0])
	}
	return cancelled
}

func (o *Orchestrator) chatGPTConversationLocked(job *chat.ConversationJob) bool {
	for _, message := range o.messages {
		if message.Sequence == job.SourceSequence {
			return message.Author == chat.ChatGPT
		}
	}
	return false
}

func (o *Orchestrator) addressChatGPT(text string, route *chat.RouteMetadata) error {
	if text == "" {
		return fmt.Errorf("message is empty")
	}
	o.mu.Lock()
	if o.closed || o.room.ChatGPT == nil || !o.room.ChatGPT.Enabled || !time.Now().Before(o.room.ChatGPT.ExpiresAt) {
		o.mu.Unlock()
		return fmt.Errorf("enable ChatGPT with /join @chatgpt before addressing it")
	}
	message, err := o.appendRoutedUserMessageLocked(chat.ChatGPT, text, nil, route, chat.WorkflowPlan, chat.InputConversation, chat.IntentHigh, "", "")
	o.mu.Unlock()
	if err == nil {
		o.send(Event{Type: EventMessage, Message: &message})
	}
	return err
}

// PublishChatGPT appends an AI-authored contribution, with optional bounded
// read-only replies. It never calls the human input router or parses commands.
// The API supplies the authenticated route and validates the participation lease.
func (o *Orchestrator) PublishChatGPT(text string, replyTo uint64, recipients []chat.Participant, route chat.RouteMetadata, options ...chat.EffortSelection) (chat.Message, bool, error) {
	return o.PublishChatGPTClass(text, replyTo, recipients, route, "", options...)
}

func (o *Orchestrator) PublishChatGPTClass(text string, replyTo uint64, recipients []chat.Participant, route chat.RouteMetadata, class chat.ConversationClass, options ...chat.EffortSelection) (chat.Message, bool, error) {
	return o.publishChatGPTCoordinated(text, replyTo, recipients, route, class, nil, "", options...)
}

func (o *Orchestrator) publishChatGPTCoordinated(text string, replyTo uint64, recipients []chat.Participant, route chat.RouteMetadata, class chat.ConversationClass, dispatch *chat.CoordinationDispatch, parent string, options ...chat.EffortSelection) (chat.Message, bool, error) {
	if class != "" && (len(recipients) == 0 || (class != chat.ConversationQuick && class != chat.ConversationResearch)) {
		return chat.Message{}, false, fmt.Errorf("reply_class requires recipients and must be quick or research")
	}
	if class == "" && len(recipients) > 0 {
		class = chat.ConversationQuick
	}
	effort, err := normalizeEffortSelection(options)
	if err != nil {
		return chat.Message{}, false, err
	}
	o.mu.Lock()
	if o.closed || o.room.ChatGPT == nil || !o.chatGPTCanPublishLocked(parent) || o.room.ChatGPT.Paused || (o.room.Coordination != nil && o.room.Coordination.State == "stopped") {
		o.mu.Unlock()
		return chat.Message{}, false, fmt.Errorf("ChatGPT participation is disconnected or paused")
	}
	for _, message := range o.messages {
		if message.Route != nil && message.Route.MessageID == route.MessageID {
			defer o.mu.Unlock()
			if message.Author != chat.ChatGPT || message.InputIntent == chat.InputWork || message.Text != text || message.ReplyTo != replyTo || !slices.Equal(message.RequestedReplies, recipients) || !message.EffortSelection.Equal(effort) || !sameReplyClass(message, class) || !reflect.DeepEqual(message.Coordination, dispatch) {
				return chat.Message{}, false, fmt.Errorf("operation id was already used with different content")
			}
			for _, job := range o.room.Conversations {
				if job.SourceSequence == message.Sequence && job.TerminalReason == "could not persist peer reply request" {
					return message, false, fmt.Errorf("contribution saved but peer replies could not be scheduled")
				}
			}
			// A retry never reschedules peer work.
			return message, false, nil
		}
	}
	if err := o.validateCoordinationDispatchLocked(dispatch, route.MessageID, "", false); err != nil {
		o.mu.Unlock()
		return chat.Message{}, false, err
	}
	if dispatch != nil && dispatch.HandoffID != "" && len(recipients) == 0 {
		o.mu.Unlock()
		return chat.Message{}, false, fmt.Errorf("a post cannot resolve a handoff")
	}
	if len(recipients) > 4 || len(text) > 16000 || strings.TrimSpace(text) == "" {
		o.mu.Unlock()
		return chat.Message{}, false, fmt.Errorf("contribution must contain 1–16000 bytes and at most four reply recipients")
	}
	pending := 0
	for i := range o.room.Conversations {
		if !o.room.Conversations[i].State.Terminal() && o.chatGPTConversationLocked(&o.room.Conversations[i]) {
			pending++
		}
	}
	if pending+len(recipients) > 4 {
		o.mu.Unlock()
		return chat.Message{}, false, fmt.Errorf("wait for the outstanding peer replies before requesting more")
	}
	seen := make(map[chat.Participant]bool)
	for _, recipient := range recipients {
		if !recipient.ValidAgent() || seen[recipient] || o.agents[recipient] == nil || !o.room.Present(recipient) {
			o.mu.Unlock()
			return chat.Message{}, false, fmt.Errorf("reply recipient must be a distinct, present local AI")
		}
		seen[recipient] = true
	}
	if err := o.validateEffortSelectionLocked(effort, recipients); err != nil {
		o.mu.Unlock()
		return chat.Message{}, false, err
	}
	if replyTo != 0 {
		found := false
		for _, message := range o.messages {
			if message.Sequence == replyTo && message.Kind == chat.MessageText && (message.Author == chat.User || message.Author == chat.ChatGPT || message.Author.ValidAgent()) {
				found = true
			}
		}
		if !found {
			o.mu.Unlock()
			return chat.Message{}, false, fmt.Errorf("reply target is not a room message")
		}
	}
	id, err := store.NewID()
	if err != nil {
		o.mu.Unlock()
		return chat.Message{}, false, err
	}
	now := time.Now().UTC()
	message := chat.Message{
		Coordination:    dispatch.Clone(),
		EffortSelection: effort.Clone(),
		ID:              id, Sequence: o.nextSequence, Author: chat.ChatGPT, Kind: chat.MessageText,
		ReplyClass: class, Text: text, ReplyTo: replyTo, RequestedReplies: append([]chat.Participant(nil), recipients...),
		WorkflowMode: chat.WorkflowPlan, DelegationPolicy: chat.DelegationManual,
		Route: &route, CreatedAt: now,
	}
	jobs := make([]chat.ConversationJob, 0, len(recipients))
	for _, recipient := range recipients {
		jobID, err := store.NewID()
		if err != nil {
			o.mu.Unlock()
			return chat.Message{}, false, err
		}
		deadline := now.Add(class.TotalBudget())
		jobs = append(jobs, chat.ConversationJob{
			EffortSelection: effort.Clone(),
			ID:              jobID, SourceSequence: message.Sequence, State: chat.ConversationFinding,
			Class: class, WorkflowMode: chat.WorkflowPlan,
			Requested: []chat.Participant{recipient}, CreatedAt: now, UpdatedAt: now,
			StartedAt: &now, Deadline: &deadline, LastActivityAt: now, RemoteMessageID: route.MessageID,
		})
	}
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		o.mu.Unlock()
		return chat.Message{}, false, fmt.Errorf("could not save ChatGPT contribution")
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	if o.chatgptPending == nil {
		o.chatgptPending = make(map[string]bool)
	}
	for _, job := range jobs {
		o.chatgptPending[job.ID] = true
	}
	o.room.Conversations = append(o.room.Conversations, jobs...)
	o.mu.Unlock()
	o.send(Event{Type: EventMessage, Message: &message})
	if err := o.saveRoom(); err != nil {
		o.mu.Lock()
		for _, job := range jobs {
			delete(o.chatgptPending, job.ID)
			if current := o.conversationLocked(job.ID); current != nil {
				current.State, current.TerminalReason = chat.ConversationFailed, "could not persist peer reply request"
			}
		}
		o.mu.Unlock()
		_ = o.saveRoom()
		return message, true, fmt.Errorf("contribution saved but peer replies could not be scheduled")
	}
	o.mu.Lock()
	for _, job := range jobs {
		delete(o.chatgptPending, job.ID)
	}
	o.mu.Unlock()
	for i := range jobs {
		job := jobs[i]
		o.send(Event{Type: EventConversation, Conversation: &job})
	}
	o.signalConversationScheduler()
	return message, true, nil
}

type chatGPTWorkSubmission struct {
	coordination *chat.CoordinationDispatch
	effort       chat.EffortSelection
	replyTo      uint64
	created      bool
	round        *chat.RoundSpec
	requested    []chat.Participant
}

// RequestChatGPTWork accepts an explicit assignment from the authorized ChatGPT
// participant. It uses the normal scheduler and the host's current mode and
// permission ceiling, retaining ChatGPT authorship instead of impersonating a
// local human. Each assignment is a distinct workflow, never implicit steering.
func (o *Orchestrator) RequestChatGPTWork(text string, target chat.Participant, replyTo uint64, route chat.RouteMetadata, options ...chat.EffortSelection) (chat.Message, bool, error) {
	effort, err := normalizeEffortSelection(options)
	if err != nil {
		return chat.Message{}, false, err
	}
	submission := &chatGPTWorkSubmission{replyTo: replyTo, effort: effort}
	return o.submitChatGPTWork(text, target, route, submission, "")
}

// RequestChatGPTRound uses the same sequential, read-only round runner as /round.
// The structured request selects a single round, not a script of future actions.
func (o *Orchestrator) RequestChatGPTRound(text string, participants []chat.Participant, replyTo uint64, route chat.RouteMetadata, options ...chat.EffortSelection) (chat.Message, bool, error) {
	effort, err := normalizeEffortSelection(options)
	if err != nil {
		return chat.Message{}, false, err
	}
	submission := &chatGPTWorkSubmission{replyTo: replyTo, requested: append([]chat.Participant(nil), participants...), round: &chat.RoundSpec{}, effort: effort}
	return o.submitChatGPTWork(text, "", route, submission, "")
}

func (o *Orchestrator) submitChatGPTWork(text string, target chat.Participant, route chat.RouteMetadata, submission *chatGPTWorkSubmission, mode chat.WorkflowMode) (chat.Message, bool, error) {
	sequence, err := o.postWorkTrackedWithOptions(text, nil, &route, target, false, chat.IntentHigh,
		workSubmissionOptions{forceNew: true, delegationPolicy: chat.DelegationManual, chatGPT: submission, modeOverride: mode})
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, message := range o.messages {
		if message.Sequence == sequence {
			return message, submission.created, err
		}
	}
	return chat.Message{}, submission.created, err
}

func (o *Orchestrator) validateChatGPTWorkLocked(text string, target chat.Participant, replyTo uint64, route *chat.RouteMetadata, effort chat.EffortSelection) (chat.Message, bool, error) {
	if o.room.ChatGPT == nil || !o.room.Present(chat.ChatGPT) || o.room.ChatGPT.Paused || (o.room.Coordination != nil && o.room.Coordination.State == "stopped") {
		return chat.Message{}, false, fmt.Errorf("ChatGPT participation is disconnected or paused")
	}
	if route == nil || route.MessageID == "" || !target.ValidAgent() || len(text) > 16000 || strings.TrimSpace(text) == "" {
		return chat.Message{}, false, fmt.Errorf("work requires a present local AI, an operation id, and 1–16000 bytes of task text")
	}
	pending := 0
	for _, message := range o.messages {
		if message.Route != nil && message.Route.MessageID == route.MessageID {
			if message.Author != chat.ChatGPT || !message.IsWorkflowSource() || message.Round != nil || message.Text != text || message.Target != target || message.ReplyTo != replyTo || !message.EffortSelection.Equal(effort) {
				return chat.Message{}, false, fmt.Errorf("operation id was already used with different content or action")
			}
			if o.room.Workflows[message.WorkflowID].WaitReason == "could not persist ChatGPT work request" {
				return message, true, fmt.Errorf("work request saved but could not be scheduled")
			}
			return message, true, nil
		}
		if message.Author == chat.ChatGPT && message.IsWorkflowSource() {
			if record, ok := o.room.Workflows[message.WorkflowID]; ok && !record.State.Terminal() {
				pending++
			}
		}
	}
	if pending >= 4 {
		return chat.Message{}, false, fmt.Errorf("wait for outstanding ChatGPT work before requesting more")
	}
	if err := o.validateEffortSelectionLocked(effort, []chat.Participant{target}); err != nil {
		return chat.Message{}, false, err
	}
	if replyTo != 0 {
		found := false
		for _, message := range o.messages {
			if message.Sequence == replyTo && message.Kind == chat.MessageText && (message.Author == chat.User || message.Author == chat.ChatGPT || message.Author.ValidAgent()) {
				found = true
				break
			}
		}
		if !found {
			return chat.Message{}, false, fmt.Errorf("reply target is not a room message")
		}
	}
	return chat.Message{}, false, nil
}

func (o *Orchestrator) validateChatGPTRoundLocked(text string, route *chat.RouteMetadata, submission *chatGPTWorkSubmission) (chat.Message, bool, error) {
	if o.room.ChatGPT == nil || !o.room.Present(chat.ChatGPT) || o.room.ChatGPT.Paused || (o.room.Coordination != nil && o.room.Coordination.State == "stopped") {
		return chat.Message{}, false, fmt.Errorf("ChatGPT participation is disconnected or paused")
	}
	if route == nil || route.MessageID == "" || len(text) > 16000 || strings.TrimSpace(text) == "" || len(submission.requested) > 4 {
		return chat.Message{}, false, fmt.Errorf("a round requires an operation id, 1–16000 bytes of discussion text, and at most four selected participants")
	}
	foundReply := submission.replyTo == 0
	for _, message := range o.messages {
		if message.Route != nil && message.Route.MessageID == route.MessageID {
			if message.Author != chat.ChatGPT || !message.IsWorkflowSource() || message.Round == nil || message.Text != text || message.ReplyTo != submission.replyTo || !slices.Equal(message.RequestedReplies, submission.requested) || !message.EffortSelection.Equal(submission.effort) {
				return chat.Message{}, false, fmt.Errorf("operation id was already used with different content or action")
			}
			if o.room.Workflows[message.WorkflowID].WaitReason == "could not persist ChatGPT work request" {
				return message, true, fmt.Errorf("round request saved but could not be scheduled")
			}
			return message, true, nil
		}
		if message.Sequence == submission.replyTo && message.Kind == chat.MessageText && (message.Author == chat.User || message.Author == chat.ChatGPT || message.Author.ValidAgent()) {
			foundReply = true
		}
	}
	if !foundReply {
		return chat.Message{}, false, fmt.Errorf("reply target is not a room message; read the exact material before requesting its review")
	}
	if o.activeWork > 0 || len(o.room.PendingInputs) > 0 {
		return chat.Message{}, false, fmt.Errorf("room work is still pending; use mohuddle_read to obtain its result before requesting a round")
	}
	for i := range o.room.Conversations {
		if !o.room.Conversations[i].State.Terminal() && o.chatGPTConversationLocked(&o.room.Conversations[i]) {
			return chat.Message{}, false, fmt.Errorf("peer replies are still pending; use mohuddle_read to obtain them before requesting a round")
		}
	}
	selected := append([]chat.Participant(nil), submission.requested...)
	if len(selected) == 0 {
		selected = o.workflowParticipantsLocked(time.Now())
	}
	moderator := o.room.Moderator
	if !moderator.ValidAgent() || !o.hasStartableCoreLocked(time.Now()) {
		return chat.Message{}, false, fmt.Errorf("a moderated round needs an active core moderator")
	}
	seen := make(map[chat.Participant]bool)
	for _, participant := range selected {
		if !participant.ValidAgent() || seen[participant] || !o.room.Present(participant) || o.agents[participant] == nil {
			return chat.Message{}, false, fmt.Errorf("round participants must be distinct, present local AI participants")
		}
		seen[participant] = true
	}
	ordered := append(withoutParticipant(selected, moderator), moderator)
	for _, participant := range ordered {
		eligibility := o.participantStartEligibilityLocked(participant, time.Now(), eligibilityOptions{})
		if _, temporary := o.temporary[participant]; temporary || !eligibility.Eligible {
			return chat.Message{}, false, fmt.Errorf("%s is unavailable for a round: %s", participant, eligibility.Reason)
		}
	}
	submission.round.Participants, submission.round.Moderator = ordered, moderator
	if err := o.validateEffortSelectionLocked(submission.effort, ordered); err != nil {
		return chat.Message{}, false, err
	}
	return chat.Message{}, false, nil
}

func (o *Orchestrator) failChatGPTWorkPersistence(workflowID string, version, sequence uint64) {
	o.mu.Lock()
	delete(o.chatgptPending, workflowID)
	if runtime, ok := o.workflows[version]; ok && runtime.id == workflowID {
		delete(o.workflows, version)
	}
	delete(o.workflowVersions, workflowID)
	o.room.PendingInputs = removeSequence(o.room.PendingInputs, sequence)
	record := o.room.Workflows[workflowID]
	record.State, record.WaitReason = chat.WorkflowCancelled, "could not persist ChatGPT work request"
	record.UpdatedAt = time.Now().UTC()
	o.room.Workflows[workflowID] = record
	queued := len(o.room.PendingInputs)
	o.mu.Unlock()
	_ = o.saveRoom()
	o.send(Event{Type: EventQueueChanged, Queued: queued})
}

func (o *Orchestrator) appendChatGPTWorkMessageLocked(text string, target chat.Participant, route chat.RouteMetadata, mode chat.WorkflowMode, workflowID string, policy chat.DelegationPolicy, submission *chatGPTWorkSubmission) (chat.Message, error) {
	id, err := store.NewID()
	if err != nil {
		return chat.Message{}, err
	}
	message := chat.Message{
		Coordination:    submission.coordination.Clone(),
		EffortSelection: submission.effort.Clone(),
		ID:              id, Sequence: o.nextSequence, Author: chat.ChatGPT, Target: target, Kind: chat.MessageText,
		Text: text, ReplyTo: submission.replyTo, InputIntent: chat.InputWork, IntentConfidence: chat.IntentHigh,
		WorkflowID: workflowID, WorkflowMode: mode, DelegationPolicy: policy, Route: &route, CreatedAt: time.Now().UTC(),
		Round: submission.round, RequestedReplies: append([]chat.Participant(nil), submission.requested...),
	}
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		return chat.Message{}, fmt.Errorf("could not save ChatGPT work request")
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	return message, nil
}

func sameReplyClass(m chat.Message, c chat.ConversationClass) bool {
	old := m.ReplyClass
	if old == "" && len(m.RequestedReplies) > 0 {
		old = chat.ConversationQuick
	}
	return old == c
}

func (o *Orchestrator) ScheduleChatGPT(a chat.ChatGPTAssignment) (chat.Message, bool, error) {
	switch a.Kind {
	case "work":
		return o.submitChatGPTWork(a.Text, a.Target, a.Route, &chatGPTWorkSubmission{replyTo: a.ReplyTo, effort: a.Effort, coordination: a.Coordination.Clone()}, "")
	case "round":
		return o.submitChatGPTWork(a.Text, "", a.Route, &chatGPTWorkSubmission{replyTo: a.ReplyTo, effort: a.Effort, coordination: a.Coordination.Clone(), requested: append([]chat.Participant(nil), a.Participants...), round: &chat.RoundSpec{}}, "")
	default:
		return o.publishChatGPTCoordinated(a.Text, a.ReplyTo, a.Participants, a.Route, a.Class, a.Coordination, "", a.Effort)
	}
}

func (o *Orchestrator) chatGPTCanPublishLocked(parent string) bool {
	if parent == "" {
		return o.room.Present(chat.ChatGPT)
	}
	c := continuation(o.room.Coordination, parent)
	return c != nil && c.State == "queued" && o.room.Coordination.State == "pending" && o.room.ChatGPT.Enabled && time.Now().Before(o.room.ChatGPT.ExpiresAt) && o.room.ChatGPT.PauseReason != "no_progress"
}

func (o *Orchestrator) validateCoordinationDispatchLocked(d *chat.CoordinationDispatch, messageID string, target chat.Participant, work bool) error {
	if d == nil {
		return nil
	}
	r := o.room.Coordination
	if r == nil || r.ID != d.RunID || r.State == "stopped" {
		return fmt.Errorf("current monitored run required")
	}
	if d.HandoffID != "" {
		h := handoff(r, d.HandoffID)
		if h == nil {
			return fmt.Errorf("unknown handoff; read current coordination state")
		}
		if !h.Open() && h.Resolution != "coordinator_reported_blocked" {
			return fmt.Errorf("handoff is already resolved")
		}
		for _, m := range o.messages {
			if m.Coordination != nil && m.Coordination.RunID == d.RunID && m.Coordination.HandoffID == d.HandoffID && m.Route != nil && m.Route.MessageID != messageID {
				return fmt.Errorf("handoff already has an assignment; inspect its outcome")
			}
		}
	}
	if spec := d.Continuation; spec != nil {
		if !work || !spec.Target.ValidAgent() || spec.Target == target || !o.room.Present(spec.Target) || o.agents[spec.Target] == nil {
			return fmt.Errorf("continuation requires a distinct present reviewer on a work request")
		}
		if strings.TrimSpace(spec.Text) == "" || len(spec.Text) > 8000 {
			return fmt.Errorf("continuation task must contain 1–8000 bytes")
		}
		if spec.ReplyClass != "" && spec.ReplyClass != chat.ConversationQuick && spec.ReplyClass != chat.ConversationResearch {
			return fmt.Errorf("invalid continuation reply class")
		}
		effort := chat.EffortSelection{}
		if spec.Effort != "" {
			effort.Efforts = map[chat.Participant]string{spec.Target: spec.Effort}
		}
		if err := o.validateEffortSelectionLocked(effort, []chat.Participant{spec.Target}); err != nil {
			return err
		}
	}
	return nil
}
