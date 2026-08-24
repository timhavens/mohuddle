package room

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

const (
	defaultTemporaryResponders = 2
	maxTemporaryResponders     = 8
	temporaryGracePeriod       = 5 * time.Minute
)

type conversationLaunch struct {
	id          string
	participant chat.Participant
	attempt     int
	temporary   bool
}

func (o *Orchestrator) startConversation(text string, attachments []chat.Attachment, route *chat.RouteMetadata, requested []chat.Participant, class chat.ConversationClass) error {
	text = strings.TrimSpace(text)
	if text == "" && len(attachments) == 0 {
		return fmt.Errorf("message is empty")
	}
	id, err := store.NewID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	deadline := now.Add(class.TotalBudget())
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	mode := o.room.WorkflowMode.WithDefault()
	message, err := o.appendRoutedUserMessageLocked("", text, attachments, route, mode, chat.InputConversation, chat.IntentHigh, id)
	if err != nil {
		o.mu.Unlock()
		return err
	}
	remoteID := ""
	if route != nil {
		remoteID = route.MessageID
	}
	job := chat.ConversationJob{
		ID: id, SourceSequence: message.Sequence, State: chat.ConversationFinding, Class: class,
		WorkflowMode: mode, Requested: append([]chat.Participant(nil), requested...),
		CreatedAt: now, UpdatedAt: now, StartedAt: &now, Deadline: &deadline,
		LastActivityAt: now, RemoteMessageID: remoteID,
	}
	o.room.Conversations = append(o.room.Conversations, job)
	o.mu.Unlock()

	o.send(Event{Type: EventMessage, Message: &message})
	copy := cloneConversationJobs([]chat.ConversationJob{job})[0]
	o.send(Event{Type: EventConversation, Conversation: &copy})
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.signalConversationScheduler()
	return nil
}

func (o *Orchestrator) persistAmbiguousInput(text string, attachments []chat.Attachment, route *chat.RouteMetadata, target chat.Participant, class chat.ConversationClass) error {
	return o.persistPendingRouteInput(text, attachments, route, target, chat.InputAmbiguous, chat.IntentLow, class)
}

func (o *Orchestrator) persistPendingRouteInput(text string, attachments []chat.Attachment, route *chat.RouteMetadata, target chat.Participant, intent chat.InputIntent, confidence chat.IntentConfidence, class chat.ConversationClass) error {
	id, err := store.NewID()
	if err != nil {
		return err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("room is closed")
	}
	mode := o.room.WorkflowMode.WithDefault()
	message, err := o.appendRoutedUserMessageLocked(target, text, attachments, route, mode, intent, confidence, id)
	if err == nil {
		o.room.PendingRoutes = append(o.room.PendingRoutes, message.Sequence)
	}
	pendingRoutes := len(o.room.PendingRoutes)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	o.send(Event{Type: EventMessage, Message: &message})
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Text: fmt.Sprintf("%d message(s) need routing", pendingRoutes)})
	_ = class // retained by the source text and recalculated after the human routes it
	return nil
}

func (o *Orchestrator) appendRoutedUserMessageLocked(target chat.Participant, text string, attachments []chat.Attachment, route *chat.RouteMetadata, mode chat.WorkflowMode, intent chat.InputIntent, confidence chat.IntentConfidence, conversationID string) (chat.Message, error) {
	message := chat.Message{
		Sequence: o.nextSequence, Author: chat.User, Target: target, Kind: chat.MessageText,
		WorkflowMode: mode.WithDefault(), InputIntent: intent, IntentConfidence: confidence,
		ConversationID: conversationID, Text: strings.TrimSpace(text),
		Attachments: append([]chat.Attachment(nil), attachments...), CreatedAt: time.Now().UTC(),
	}
	if route != nil {
		copy := *route
		copy.Hops = append([]string(nil), route.Hops...)
		message.Route = &copy
	}
	id, err := store.NewID()
	if err != nil {
		return chat.Message{}, err
	}
	message.ID = id
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		return chat.Message{}, err
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	return message, nil
}

func (o *Orchestrator) ConversationJobs() []chat.ConversationJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	return cloneConversationJobs(o.room.Conversations)
}

func (o *Orchestrator) PendingRouteCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.room.PendingRoutes)
}

func (o *Orchestrator) signalConversationScheduler() {
	select {
	case o.conversationWake <- struct{}{}:
	default:
	}
}

func (o *Orchestrator) runConversationScheduler() {
	defer o.schedulerWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-o.lifetime.Done():
			return
		case <-o.conversationWake:
			o.scheduleConversations()
		case <-ticker.C:
			o.scheduleConversations()
		}
	}
}

func (o *Orchestrator) scheduleConversations() {
	now := time.Now().UTC()
	var launches []conversationLaunch
	var changed []chat.ConversationJob
	var closing []agent.Agent

	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	if !o.conversationRouting {
		o.mu.Unlock()
		return
	}

	for index := range o.room.Conversations {
		job := &o.room.Conversations[index]
		if job.Deadline != nil && !job.State.Terminal() && !now.Before(*job.Deadline) {
			if turn := o.activeTurns[job.Assigned]; turn.conversationID == job.ID && turn.cancel != nil {
				turn.cancel()
			}
			job.State = chat.ConversationNeedsAttention
			job.TerminalReason = "response deadline expired"
			job.Assigned = ""
			job.QueuePosition = 0
			job.UpdatedAt = now
			if job.Temporary {
				job.RetireAt = &now
			}
			changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
		}
	}

	for participant, conversationID := range o.temporary {
		job := o.conversationLocked(conversationID)
		if job == nil || job.RetireAt == nil || now.Before(*job.RetireAt) || o.activeTurns[participant].cancel != nil {
			continue
		}
		if runner := o.agents[participant]; runner != nil {
			closing = append(closing, runner)
		}
		delete(o.temporary, participant)
		delete(o.agents, participant)
		delete(o.settings, participant)
		delete(o.agentGates, participant)
		delete(o.room.Members, participant)
		delete(o.room.Sessions, participant)
		job.RetiredAt = &now
		job.UpdatedAt = now
		if job.Assigned == participant {
			job.Assigned = ""
			job.Temporary = false
			job.RetireAt = nil
		}
		changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
	}

	queuePosition := 0
	for index := range o.room.Conversations {
		job := &o.room.Conversations[index]
		if job.State.Terminal() || job.State == chat.ConversationAnswering || job.State == chat.ConversationRetrying {
			continue
		}
		participant, temporary := o.selectConversationResponderLocked(job, now)
		if !participant.ValidAgent() {
			queuePosition++
			if job.State != chat.ConversationWaiting || job.QueuePosition != queuePosition {
				job.State = chat.ConversationWaiting
				job.QueuePosition = queuePosition
				job.UpdatedAt = now
				changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
			}
			continue
		}
		if job.StartedAt == nil {
			started := now
			deadline := now.Add(job.Class.TotalBudget())
			job.StartedAt = &started
			job.Deadline = &deadline
		}
		remaining := job.Deadline.Sub(now)
		attemptBudget := job.Class.AttemptBudget()
		if remaining < attemptBudget {
			attemptBudget = remaining
		}
		attemptDeadline := now.Add(attemptBudget)
		job.Attempts = append(job.Attempts, chat.ConversationAttempt{
			Participant: participant, Provider: participant.Provider(), StartedAt: now, Deadline: attemptDeadline,
		})
		job.Assigned = participant
		job.Temporary = temporary
		job.RetireAt = nil
		job.RetiredAt = nil
		job.QueuePosition = 0
		if len(job.Attempts) == 1 {
			job.State = chat.ConversationAnswering
		} else {
			job.State = chat.ConversationRetrying
		}
		job.UpdatedAt = now
		launches = append(launches, conversationLaunch{id: job.ID, participant: participant, attempt: len(job.Attempts) - 1, temporary: temporary})
		changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
		o.wg.Add(1)
	}
	o.mu.Unlock()

	for _, runner := range closing {
		_ = runner.Close()
	}
	if len(changed) > 0 || len(closing) > 0 {
		if err := o.saveRoom(); err != nil {
			o.send(Event{Type: EventError, Err: fmt.Errorf("save conversation scheduler state: %w", err)})
		}
	}
	for _, job := range changed {
		copy := job
		o.send(Event{Type: EventConversation, Conversation: &copy})
	}
	for _, launch := range launches {
		go o.runConversationAttempt(launch)
	}
}

func (o *Orchestrator) conversationLocked(id string) *chat.ConversationJob {
	for index := range o.room.Conversations {
		if o.room.Conversations[index].ID == id {
			return &o.room.Conversations[index]
		}
	}
	return nil
}

func (o *Orchestrator) selectConversationResponderLocked(job *chat.ConversationJob, now time.Time) (chat.Participant, bool) {
	usedProviders := make(map[chat.Participant]bool, len(job.Attempts))
	for _, attempt := range job.Attempts {
		usedProviders[attempt.Provider] = true
	}
	busyProvider := make(map[chat.Participant]bool)
	for participant, turn := range o.activeTurns {
		if turn.cancel != nil {
			busyProvider[participant.Provider()] = true
		}
	}
	cores := make(map[chat.Participant]bool)
	reservedProviders := make(map[chat.Participant]bool)
	for _, participant := range o.activePresentCoreParticipantsLocked(now) {
		cores[participant] = true
		reservedProviders[participant.Provider()] = true
	}
	eligible := func(participant chat.Participant) bool {
		if conversationID, temporary := o.temporary[participant]; temporary && conversationID != job.ID {
			return false
		}
		if !o.participantOperationalLocked(participant, now) || usedProviders[participant.Provider()] || o.delegated[participant] || o.activeTurns[participant].cancel != nil {
			return false
		}
		if o.activeWork > 0 && (cores[participant] || reservedProviders[participant.Provider()] || busyProvider[participant.Provider()]) {
			return false
		}
		return true
	}
	for _, requested := range job.Requested {
		if eligible(requested) {
			return requested, o.temporary[requested] == job.ID
		}
	}
	participants := o.operationalParticipantsLocked(now)
	for _, participant := range participants {
		if participant.IsAuxiliary() && eligible(participant) {
			return participant, o.temporary[participant] == job.ID
		}
	}
	for _, participant := range participants {
		if !cores[participant] && eligible(participant) {
			return participant, o.temporary[participant] == job.ID
		}
	}
	if o.activeWork == 0 {
		for _, participant := range participants {
			if eligible(participant) {
				return participant, o.temporary[participant] == job.ID
			}
		}
	}
	if o.temporaryFactory == nil || o.temporaryLimit <= 0 || len(o.temporary) >= o.temporaryLimit {
		return "", false
	}
	for _, provider := range o.temporaryFactory.Providers() {
		if !provider.IsPrimaryAgent() || usedProviders[provider] || busyProvider[provider] || (o.activeWork > 0 && reservedProviders[provider]) || o.providerHasTemporaryLocked(provider) {
			continue
		}
		if availability, unavailable := o.room.Availability[provider]; unavailable && (availability.RetryAt == nil || now.Before(*availability.RetryAt)) {
			continue
		}
		participant := o.nextTemporaryParticipantLocked(provider)
		runner, err := o.temporaryFactory.Create(provider, participant)
		if err != nil || runner == nil {
			continue
		}
		o.agents[participant] = runner
		o.agentGates[participant] = &sync.Mutex{}
		o.settings[participant] = chat.AgentSettings{Permissions: chat.PermissionReadOnly}
		o.room.Members[participant] = true
		o.room.Sessions[participant] = chat.AgentSession{}
		o.temporary[participant] = job.ID
		return participant, true
	}
	return "", false
}

func (o *Orchestrator) providerHasTemporaryLocked(provider chat.Participant) bool {
	for participant := range o.temporary {
		if participant.Provider() == provider {
			return true
		}
	}
	return false
}

func (o *Orchestrator) nextTemporaryParticipantLocked(provider chat.Participant) chat.Participant {
	for index := 1; ; index++ {
		participant, _ := chat.AuxiliaryParticipant(provider, index)
		if o.agents[participant] == nil {
			return participant
		}
	}
}

func (o *Orchestrator) runConversationAttempt(launch conversationLaunch) {
	defer o.wg.Done()

	o.mu.Lock()
	job := o.conversationLocked(launch.id)
	gate := o.agentGates[launch.participant]
	runner := o.agents[launch.participant]
	if job == nil || gate == nil || runner == nil || launch.attempt >= len(job.Attempts) {
		o.mu.Unlock()
		return
	}
	attemptDeadline := job.Attempts[launch.attempt].Deadline
	o.mu.Unlock()

	gate.Lock()
	defer gate.Unlock()
	ctx, cancel := context.WithDeadline(o.lifetime, attemptDeadline)
	o.mu.Lock()
	job = o.conversationLocked(launch.id)
	if o.closed || job == nil || job.State.Terminal() || job.Assigned != launch.participant {
		o.mu.Unlock()
		cancel()
		return
	}
	o.activeTurns[launch.participant] = activeTurn{conversationID: launch.id, cancel: cancel}
	through := uint64(0)
	if len(o.messages) > 0 {
		through = o.messages[len(o.messages)-1].Sequence
	}
	o.mu.Unlock()
	o.send(Event{Type: EventTurnStarted, Participant: launch.participant, Role: "conversation responder", Task: o.conversationTask(launch.id), WorkflowMode: chat.WorkflowPlan})

	var draftMu sync.Mutex
	var draft strings.Builder
	emit := o.conversationEmitter(ctx, launch.participant, launch.id, &draftMu, &draft)
	spec := turnSpec{
		through: through, readOnly: true, ephemeral: true, conversationID: launch.id,
		role: "conversation responder", publicResponseRequired: true,
		instruction: "Answer this linked room conversation directly and concisely. This is chat-only and strictly read-only: do not mutate files or external state, do not claim implementation occurred, and set requires_work:true in the private marker if the human is actually asking for work to be planned or implemented.",
	}
	request := o.turnRequest(launch.participant, spec, nil)
	result, err := runner.Run(ctx, request, emit)
	if err == nil && ctx.Err() == nil {
		result, request, err = o.completeResearch(ctx, launch.participant, runner, request, result, emit, &draftMu, &draft)
	}
	_ = request
	contextErr := ctx.Err()
	cancel()
	o.finishConversationAttempt(launch, result, err, contextErr)
}

func (o *Orchestrator) conversationTask(id string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	job := o.conversationLocked(id)
	if job == nil {
		return "room conversation"
	}
	for _, message := range o.messages {
		if message.Sequence == job.SourceSequence {
			return truncateUTF8Prefix(strings.Join(strings.Fields(message.Text), " "), 120)
		}
	}
	return "room conversation"
}

func (o *Orchestrator) finishConversationAttempt(launch conversationLaunch, result agent.TurnResult, runErr, contextErr error) {
	now := time.Now().UTC()
	var message *chat.Message
	var jobCopy *chat.ConversationJob
	var availabilityErr error
	var correctionWarnings []string

	o.mu.Lock()
	if turn, ok := o.activeTurns[launch.participant]; ok && turn.conversationID == launch.id {
		delete(o.activeTurns, launch.participant)
	}
	job := o.conversationLocked(launch.id)
	if job == nil || launch.attempt >= len(job.Attempts) {
		o.mu.Unlock()
		o.send(Event{Type: EventTurnFinished, Participant: launch.participant})
		return
	}
	attempt := &job.Attempts[launch.attempt]
	attempt.CompletedAt = &now
	if job.State == chat.ConversationCancelled || job.State == chat.ConversationNeedsAttention {
		job.UpdatedAt = now
		if job.Temporary {
			job.RetireAt = &now
		}
	} else if runErr == nil && contextErr == nil && strings.TrimSpace(result.Text) != "" && !result.RequiresWork && result.AccessRequest == nil {
		appended, warnings, err := o.appendConversationAgentMessageLocked(launch.participant, launch.id, result.Text, result, job.SourceSequence)
		if err != nil {
			runErr = err
		} else {
			correctionWarnings = warnings
			message = &appended
			job.State = chat.ConversationAnswered
			job.AnswerSequence = appended.Sequence
			job.Unread = true
			job.TerminalReason = ""
			job.Assigned = launch.participant
			job.LastActivityAt = now
			if job.Temporary {
				retire := now.Add(temporaryGracePeriod)
				job.RetireAt = &retire
			}
		}
	} else if result.RequiresWork || result.AccessRequest != nil {
		job.State = chat.ConversationNeedsAttention
		job.TerminalReason = "This asks for work; choose Add to work or Replace current work. No work was implemented."
		job.Assigned = ""
		if !containsSequence(o.room.PendingRoutes, job.SourceSequence) {
			o.room.PendingRoutes = append(o.room.PendingRoutes, job.SourceSequence)
		}
		if job.Temporary {
			job.RetireAt = &now
		}
	} else {
		if runErr != nil {
			attempt.Error = runErr.Error()
			availabilityErr = runErr
		} else if contextErr != nil {
			attempt.Error = "response attempt timed out"
		} else {
			attempt.Error = "responder returned no public answer"
		}
		if len(job.Attempts) < 2 && job.Deadline != nil && now.Before(*job.Deadline) {
			job.State = chat.ConversationFinding
			job.Assigned = ""
			job.Temporary = false
			if launch.temporary {
				job.RetireAt = &now
			}
		} else {
			job.State = chat.ConversationNeedsAttention
			job.TerminalReason = attempt.Error
			job.Assigned = ""
			if temporaryConversation := o.temporary[launch.participant]; temporaryConversation == job.ID {
				retire := now
				job.RetireAt = &retire
			}
		}
	}
	job.QueuePosition = 0
	job.UpdatedAt = now
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	jobCopy = &copy
	o.mu.Unlock()

	if message != nil {
		o.send(Event{Type: EventMessage, Message: message})
	}
	for _, warning := range correctionWarnings {
		o.send(Event{Type: EventWarning, Participant: launch.participant, Text: warning})
	}
	if err := o.saveRoom(); err != nil {
		o.send(Event{Type: EventError, Participant: launch.participant, Err: fmt.Errorf("save conversation result: %w", err)})
	}
	if availabilityErr != nil && !errors.Is(availabilityErr, context.Canceled) && !errors.Is(availabilityErr, context.DeadlineExceeded) {
		availabilityParticipant := launch.participant
		if launch.temporary {
			availabilityParticipant = launch.participant.Provider()
		}
		o.recordProviderAvailability(availabilityParticipant, availabilityErr)
	}
	o.send(Event{Type: EventTurnFinished, Participant: launch.participant})
	o.send(Event{Type: EventConversation, Conversation: jobCopy})
	o.signalConversationScheduler()
}

func containsSequence(values []uint64, sequence uint64) bool {
	for _, value := range values {
		if value == sequence {
			return true
		}
	}
	return false
}

func (o *Orchestrator) CancelConversation(id string) error {
	now := time.Now().UTC()
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	if job.State.Terminal() && job.State != chat.ConversationNeedsAttention {
		o.mu.Unlock()
		return nil
	}
	if turn := o.activeTurns[job.Assigned]; turn.conversationID == id && turn.cancel != nil {
		turn.cancel()
	}
	job.State = chat.ConversationCancelled
	job.TerminalReason = "cancelled by the human"
	job.Unread = false
	job.UpdatedAt = now
	if job.Temporary {
		job.RetireAt = &now
	}
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Conversation: &copy})
	o.signalConversationScheduler()
	return nil
}

func (o *Orchestrator) RetryConversation(id string) error {
	now := time.Now().UTC()
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	if job.State != chat.ConversationNeedsAttention {
		o.mu.Unlock()
		return fmt.Errorf("conversation is not waiting for a retry")
	}
	job.State = chat.ConversationFinding
	job.Assigned = ""
	job.Attempts = nil
	job.StartedAt = &now
	deadline := now.Add(job.Class.TotalBudget())
	job.Deadline = &deadline
	job.RetireAt = nil
	job.TerminalReason = ""
	job.UpdatedAt = now
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Conversation: &copy})
	o.signalConversationScheduler()
	return nil
}

// KeepWaiting extends a conversation's advertised total budget without
// erasing its attempt history. This is a human-authorized extension after the
// normal bounded response window has expired.
func (o *Orchestrator) KeepWaitingConversation(id string) error {
	now := time.Now().UTC()
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	if job.State != chat.ConversationNeedsAttention {
		o.mu.Unlock()
		return fmt.Errorf("conversation is not waiting for direction")
	}
	job.State = chat.ConversationFinding
	job.Assigned = ""
	job.StartedAt = &now
	deadline := now.Add(job.Class.TotalBudget())
	job.Deadline = &deadline
	job.RetireAt = nil
	job.TerminalReason = ""
	job.UpdatedAt = now
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Conversation: &copy})
	o.signalConversationScheduler()
	return nil
}

func (o *Orchestrator) AcknowledgeConversation(id string) error {
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	job.Unread = false
	job.UpdatedAt = time.Now().UTC()
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Conversation: &copy})
	return nil
}

func (o *Orchestrator) ResolveInput(sequence uint64, intent chat.InputIntent, replace bool) error {
	if intent != chat.InputConversation && intent != chat.InputWork {
		return fmt.Errorf("routing choice must be conversation or work")
	}
	o.mu.Lock()
	if !containsSequence(o.room.PendingRoutes, sequence) {
		o.mu.Unlock()
		return fmt.Errorf("message %d is not waiting for routing", sequence)
	}
	var source chat.Message
	for _, message := range o.messages {
		if message.Sequence == sequence && message.Author == chat.User {
			source = cloneMessages([]chat.Message{message})[0]
			break
		}
	}
	if source.Sequence == 0 {
		o.mu.Unlock()
		return fmt.Errorf("message %d was not found", sequence)
	}
	o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, sequence)
	if intent == chat.InputConversation {
		now := time.Now().UTC()
		_, _, class := chat.ClassifyInput(source.Text, len(source.Attachments) > 0)
		deadline := now.Add(class.TotalBudget())
		job := chat.ConversationJob{
			ID: source.ConversationID, SourceSequence: source.Sequence, State: chat.ConversationFinding,
			Class: class, WorkflowMode: source.WorkflowMode.WithDefault(), CreatedAt: now, UpdatedAt: now,
			StartedAt: &now, Deadline: &deadline, LastActivityAt: now,
		}
		if source.Target.ValidAgent() {
			job.Requested = []chat.Participant{source.Target}
		}
		if source.Route != nil {
			job.RemoteMessageID = source.Route.MessageID
		}
		o.room.Conversations = append(o.room.Conversations, job)
		copy := cloneConversationJobs([]chat.ConversationJob{job})[0]
		o.mu.Unlock()
		if err := o.saveRoom(); err != nil {
			return err
		}
		o.send(Event{Type: EventConversation, Conversation: &copy})
		o.signalConversationScheduler()
		return nil
	}
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	return o.postWork(source.Text, source.Attachments, nil, source.Target, replace, chat.IntentHigh, source.WorkflowMode)
}

func removeSequence(values []uint64, target uint64) []uint64 {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func (o *Orchestrator) PromoteConversation(id string, replace bool) error {
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	var source chat.Message
	for _, message := range o.messages {
		if message.Sequence == job.SourceSequence {
			source = cloneMessages([]chat.Message{message})[0]
			break
		}
	}
	o.mu.Unlock()
	if source.Sequence == 0 {
		return fmt.Errorf("conversation source message was not found")
	}
	promotedSequence, err := o.postWorkTracked(source.Text, source.Attachments, nil, source.Target, replace, chat.IntentHigh, source.WorkflowMode)
	if err != nil {
		return err
	}
	o.mu.Lock()
	job = o.conversationLocked(id)
	if job != nil {
		job.PromotedSequence = promotedSequence
		job.UpdatedAt = time.Now().UTC()
	}
	o.mu.Unlock()
	return o.saveRoom()
}

func (o *Orchestrator) CancelPendingRoute(sequence uint64) error {
	o.mu.Lock()
	if !containsSequence(o.room.PendingRoutes, sequence) {
		o.mu.Unlock()
		return fmt.Errorf("message %d is not waiting for routing", sequence)
	}
	o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, sequence)
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, Text: "routing choice cancelled"})
	return nil
}

func (o *Orchestrator) FollowUpConversation(id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("follow-up text is required")
	}
	now := time.Now().UTC()
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	message, err := o.appendConversationMessageLocked(chat.User, chat.MessageText, id, text)
	if err == nil {
		job.State = chat.ConversationFinding
		job.Unread = false
		job.AnswerSequence = 0
		job.Attempts = nil
		job.StartedAt = &now
		deadline := now.Add(job.Class.TotalBudget())
		job.Deadline = &deadline
		job.RetireAt = nil
		job.TerminalReason = ""
		job.LastActivityAt = now
		job.UpdatedAt = now
	}
	var copy chat.ConversationJob
	if err == nil {
		copy = cloneConversationJobs([]chat.ConversationJob{*job})[0]
	}
	o.mu.Unlock()
	if err != nil {
		return err
	}
	o.send(Event{Type: EventMessage, Message: &message})
	o.send(Event{Type: EventConversation, Conversation: &copy})
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.signalConversationScheduler()
	return nil
}

func (o *Orchestrator) appendConversationAgentMessageLocked(participant chat.Participant, conversationID, text string, result agent.TurnResult, seenThrough uint64) (chat.Message, []string, error) {
	message := chat.Message{
		Sequence: o.nextSequence, Author: participant, Kind: chat.MessageText,
		ConversationID: conversationID, Text: strings.TrimSpace(text), CreatedAt: time.Now().UTC(),
	}
	correctionEvents, warnings := o.correctionEventsLocked(participant, result, message.Sequence, seenThrough)
	message.CorrectionEvents = correctionEvents
	id, err := store.NewID()
	if err != nil {
		return chat.Message{}, nil, err
	}
	message.ID = id
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		return chat.Message{}, nil, err
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	return message, warnings, nil
}

func (o *Orchestrator) conversationEmitter(ctx context.Context, participant chat.Participant, conversationID string, draftMu *sync.Mutex, draft *strings.Builder) func(agent.Event) {
	return func(event agent.Event) {
		if ctx.Err() != nil {
			return
		}
		event.Agent = participant
		if event.Type == agent.EventDelta {
			draftMu.Lock()
			draft.WriteString(event.Text)
			draftMu.Unlock()
		}
		if event.Type == agent.EventTool && strings.TrimSpace(event.Text) != "" {
			o.mu.Lock()
			message, err := o.appendConversationMessageLocked(participant, chat.MessageTool, conversationID, event.Text)
			o.mu.Unlock()
			if err != nil {
				o.send(Event{Type: EventError, Participant: participant, Err: err})
			} else {
				o.send(Event{Type: EventMessage, Message: &message})
			}
		}
		o.send(Event{Type: EventAgent, AgentEvent: &event})
	}
}

func (o *Orchestrator) appendConversationMessageLocked(author chat.Participant, kind chat.MessageKind, conversationID, text string) (chat.Message, error) {
	message := chat.Message{Sequence: o.nextSequence, Author: author, Kind: kind, ConversationID: conversationID, Text: strings.TrimSpace(text), CreatedAt: time.Now().UTC()}
	id, err := store.NewID()
	if err != nil {
		return chat.Message{}, err
	}
	message.ID = id
	if err := o.store.AppendMessage(o.room.ID, message); err != nil {
		return chat.Message{}, err
	}
	o.nextSequence++
	o.messages = append(o.messages, message)
	return message, nil
}
