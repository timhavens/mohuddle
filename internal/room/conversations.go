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
	message, err := o.appendRoutedUserMessageLocked("", text, attachments, route, mode, chat.InputConversation, chat.IntentHigh, id, "")
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
		failedAt := time.Now().UTC()
		o.mu.Lock()
		current := o.conversationLocked(id)
		var failureMessage *chat.Message
		if current != nil {
			failureMessage = o.failConversationLocked(current, "could not persist conversation start: "+err.Error(), failureLineNotStarted, failedAt)
			copy = cloneConversationJobs([]chat.ConversationJob{*current})[0]
		}
		o.mu.Unlock()
		if failureMessage != nil {
			o.send(Event{Type: EventMessage, Message: failureMessage})
		}
		_ = o.saveRoom()
		o.send(Event{Type: EventConversation, Conversation: &copy})
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
	delegationPolicy := resolvedDelegationPolicy(o.room.DelegationPolicy, mode, target.ValidAgent(), delegationDefault)
	message, err := o.appendRoutedUserMessageLocked(target, text, attachments, route, mode, intent, confidence, id, "", delegationPolicy)
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

func (o *Orchestrator) appendRoutedUserMessageLocked(target chat.Participant, text string, attachments []chat.Attachment, route *chat.RouteMetadata, mode chat.WorkflowMode, intent chat.InputIntent, confidence chat.IntentConfidence, conversationID, workflowID string, delegationPolicies ...chat.DelegationPolicy) (chat.Message, error) {
	delegationPolicy := chat.DelegationManual
	if len(delegationPolicies) > 0 {
		delegationPolicy = delegationPolicies[0]
	}
	message := chat.Message{
		Sequence: o.nextSequence, Author: chat.User, Target: target, Kind: chat.MessageText,
		WorkflowID:   workflowID,
		WorkflowMode: mode.WithDefault(), DelegationPolicy: delegationPolicy, InputIntent: intent, IntentConfidence: confidence,
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
	var failureMessages []chat.Message

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
			if message := o.failConversationLocked(job, "hard response deadline expired", failureLineTimedOut, now); message != nil {
				failureMessages = append(failureMessages, *message)
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
		}
		job.Temporary = false
		job.RetireAt = nil
		changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
	}

	queuePosition := 0
	for index := range o.room.Conversations {
		job := &o.room.Conversations[index]
		if job.State.Terminal() || job.State == chat.ConversationAnswering || job.State == chat.ConversationRetrying {
			continue
		}
		participant, temporary, waitReason := o.selectConversationResponderLocked(job, now)
		if !participant.ValidAgent() {
			queuePosition++
			if job.State != chat.ConversationWaiting || job.QueuePosition != queuePosition || job.WaitReason != waitReason {
				job.State = chat.ConversationWaiting
				job.QueuePosition = queuePosition
				job.WaitReason = waitReason
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
			Participant: participant, Provider: participant.Provider(), StartedAt: now, Deadline: attemptDeadline, Window: job.AttemptWindow,
		})
		job.Assigned = participant
		job.Temporary = temporary
		job.RetireAt = nil
		job.RetiredAt = nil
		job.QueuePosition = 0
		job.WaitReason = ""
		if conversationWindowAttemptCount(job) == 1 {
			job.State = chat.ConversationAnswering
		} else {
			job.State = chat.ConversationRetrying
		}
		job.UpdatedAt = now
		o.setActivityLocked(participant, chat.SchedulerQueued, "conversation assigned; waiting for provider call", o.conversationTaskLocked(job.ID), "conversation responder", chat.OperationRouting, "waiting for provider call", string(participant.Provider()), "assigned", &attemptDeadline)
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
	for index := range failureMessages {
		message := failureMessages[index]
		o.send(Event{Type: EventMessage, Message: &message})
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

func (o *Orchestrator) selectConversationResponderLocked(job *chat.ConversationJob, now time.Time) (chat.Participant, bool, string) {
	usedProviders := make(map[chat.Participant]bool, len(job.Attempts))
	for _, attempt := range job.Attempts {
		if attempt.Window == job.AttemptWindow {
			usedProviders[attempt.Provider] = true
		}
	}
	cores := make(map[chat.Participant]bool)
	for _, participant := range o.activePresentCoreParticipantsLocked(now) {
		cores[participant] = true
	}
	lastReason := "no eligible provider is currently available"
	eligible := func(participant chat.Participant) bool {
		if usedProviders[participant.Provider()] {
			lastReason = "waiting for an alternate eligible provider"
			return false
		}
		eligibility := o.providerEligibilityLocked(participant, now, eligibilityOptions{conversationID: job.ID})
		if !eligibility.Eligible {
			if strings.TrimSpace(eligibility.Reason) != "" {
				lastReason = eligibility.Reason
			}
			return false
		}
		return true
	}
	for _, requested := range job.Requested {
		if eligible(requested) {
			return requested, o.temporary[requested] == job.ID, ""
		}
	}
	participants := o.operationalParticipantsLocked(now)
	for _, participant := range participants {
		if participant.IsAuxiliary() && eligible(participant) {
			return participant, o.temporary[participant] == job.ID, ""
		}
	}
	for _, participant := range participants {
		if !cores[participant] && eligible(participant) {
			return participant, o.temporary[participant] == job.ID, ""
		}
	}
	if o.activeWork == 0 {
		for _, participant := range participants {
			if eligible(participant) {
				return participant, o.temporary[participant] == job.ID, ""
			}
		}
	}
	if o.temporaryFactory == nil || o.temporaryLimit <= 0 || len(o.temporary) >= o.temporaryLimit {
		return "", false, lastReason
	}
	for _, provider := range o.temporaryFactory.Providers() {
		if !provider.IsPrimaryAgent() || usedProviders[provider] || o.providerHasTemporaryLocked(provider) {
			continue
		}
		eligibility := o.providerEligibilityLocked(provider, now, eligibilityOptions{temporaryRuntime: true, conversationID: job.ID})
		if !eligibility.Eligible {
			if strings.TrimSpace(eligibility.Reason) != "" {
				lastReason = eligibility.Reason
			}
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
		return participant, true, ""
	}
	return "", false, lastReason
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
	eligibility := o.participantStartEligibilityLocked(launch.participant, time.Now(), eligibilityOptions{conversationID: launch.id})
	if !eligibility.Eligible {
		now := time.Now().UTC()
		if launch.attempt == len(job.Attempts)-1 && job.Attempts[launch.attempt].CompletedAt == nil {
			job.Attempts = job.Attempts[:launch.attempt]
		}
		job.State = chat.ConversationWaiting
		job.Assigned = ""
		job.QueuePosition = 0
		job.WaitReason = eligibility.Reason
		job.UpdatedAt = now
		if launch.temporary {
			job.RetireAt = &now
		}
		copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
		o.mu.Unlock()
		cancel()
		o.setActivity(launch.participant, chat.SchedulerWaiting, "conversation assignment withdrawn", "", "conversation responder", chat.OperationWaiting, eligibility.Reason, string(launch.participant.Provider()), "assignment_withdrawn", copy.Deadline)
		if err := o.saveRoom(); err != nil {
			o.send(Event{Type: EventError, Participant: launch.participant, Err: fmt.Errorf("save withdrawn conversation assignment: %w", err)})
		}
		o.send(Event{Type: EventConversation, Conversation: &copy})
		o.signalConversationScheduler()
		if err := o.ResumeQueued(); err != nil {
			o.send(Event{Type: EventError, Err: fmt.Errorf("resume work after conversation withdrawal: %w", err)})
		}
		return
	}
	turnID, err := store.NewID()
	if err != nil {
		o.mu.Unlock()
		cancel()
		o.send(Event{Type: EventError, Participant: launch.participant, Err: fmt.Errorf("create conversation turn id: %w", err)})
		return
	}
	startedAt := time.Now().UTC()
	task := o.conversationTaskLocked(launch.id)
	o.activeTurns[launch.participant] = activeTurn{turnID: turnID, conversationID: launch.id, cancel: cancel}
	activity := o.setActivityLocked(launch.participant, chat.SchedulerActive, "provider call running", task, "conversation responder", chat.OperationOther, "", "", "provider_call_started", &attemptDeadline)
	through := uint64(0)
	if len(o.messages) > 0 {
		through = o.messages[len(o.messages)-1].Sequence
	}
	o.mu.Unlock()
	o.send(Event{Type: EventActivity, Participant: launch.participant, Activity: &activity})
	o.send(Event{Type: EventTurnStarted, TurnID: turnID, Participant: launch.participant, Role: "conversation responder", Task: task, WorkflowMode: chat.WorkflowPlan})

	capture := &turnCapture{}
	emit := o.conversationEmitter(ctx, launch.participant, launch.id, turnID, capture)
	spec := turnSpec{
		through: through, readOnly: true, ephemeral: true, conversationID: launch.id,
		role: "conversation responder", publicResponseRequired: true,
		instruction: "Answer this linked room conversation directly and concisely. This is chat-only and strictly read-only: do not mutate files or external state, do not claim implementation occurred, and set requires_work:true in the private marker if the human is actually asking for work to be planned or implemented.",
	}
	request := o.turnRequest(launch.participant, spec, nil)
	result, err := runner.Run(ctx, request, emit)
	if err == nil && ctx.Err() == nil {
		result, request, err = o.completeResearch(ctx, launch.participant, runner, request, result, emit)
	}
	_ = request
	contextErr := ctx.Err()
	cancel()
	o.finishConversationAttempt(launch, turnID, task, startedAt, capture, result, err, contextErr)
}

func (o *Orchestrator) conversationTask(id string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.conversationTaskLocked(id)
}

func (o *Orchestrator) conversationTaskLocked(id string) string {
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

func (o *Orchestrator) finishConversationAttempt(launch conversationLaunch, turnID, task string, startedAt time.Time, capture *turnCapture, result agent.TurnResult, runErr, contextErr error) {
	now := time.Now().UTC()
	providerErr := runErr
	var message *chat.Message
	var jobCopy *chat.ConversationJob
	var availabilityErr error
	var correctionWarnings []string
	var finalSequence uint64

	o.mu.Lock()
	if turn, ok := o.activeTurns[launch.participant]; ok && turn.conversationID == launch.id {
		delete(o.activeTurns, launch.participant)
	}
	job := o.conversationLocked(launch.id)
	if job == nil || launch.attempt >= len(job.Attempts) {
		o.mu.Unlock()
		record := o.completeTurnCapture(turnID, launch.participant, "conversation responder", task, startedAt, turnOutcome{participant: launch.participant, failed: true}, capture)
		o.send(Event{Type: EventTurnFinished, TurnID: turnID, Participant: launch.participant, Turn: record})
		return
	}
	attempt := &job.Attempts[launch.attempt]
	attempt.CompletedAt = &now
	if job.State.Terminal() {
		job.UpdatedAt = now
		if job.Temporary {
			job.RetireAt = &now
		}
	} else if runErr == nil && contextErr == nil && strings.TrimSpace(result.Text) != "" && !result.RequiresWork && result.AccessRequest == nil {
		appended, warnings, err := o.appendConversationAgentMessageLocked(launch.participant, launch.id, turnID, result.Text, result, job.SourceSequence)
		if err != nil {
			runErr = err
			attempt.Error = "could not persist responder answer: " + err.Error()
			message = o.failConversationLocked(job, attempt.Error, failureLineNotSaved, now)
		} else {
			correctionWarnings = warnings
			message = &appended
			finalSequence = appended.Sequence
			job.State = chat.ConversationAnswered
			job.AnswerSequence = appended.Sequence
			job.Unread = true
			job.TerminalReason = ""
			job.ActionState = ""
			job.FailureSequence = 0
			job.Assigned = launch.participant
			job.LastActivityAt = now
			if job.Temporary {
				retire := now.Add(temporaryGracePeriod)
				job.RetireAt = &retire
			}
		}
	} else if result.RequiresWork {
		job.State = chat.ConversationNeedsAttention
		job.ActionState = chat.ConversationRequiresWork
		job.TerminalReason = requiresWorkSentinel
		job.Assigned = ""
		o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
		if job.Temporary {
			job.RetireAt = &now
		}
	} else if result.AccessRequest != nil {
		attempt.Error = "conversation responder requested additional access"
		message = o.failConversationLocked(job, attempt.Error, failureLineNeedsAccess, now)
	} else {
		if runErr != nil {
			attempt.Error = runErr.Error()
			availabilityErr = providerErr
		} else if contextErr != nil {
			attempt.Error = "hard response deadline expired"
		} else {
			attempt.Error = "responder returned no public answer"
		}
		// Only a confirmed provider/process error can trigger the one automatic
		// alternate-provider attempt. Silence and elapsed fractions never do.
		if providerErr != nil && contextErr == nil && !errors.Is(providerErr, context.Canceled) && !errors.Is(providerErr, context.DeadlineExceeded) && conversationWindowAttemptCount(job) < 2 && job.Deadline != nil && now.Before(*job.Deadline) {
			job.State = chat.ConversationFinding
			job.ActionState = ""
			job.Assigned = ""
			job.Temporary = false
			if launch.temporary {
				job.RetireAt = &now
			}
		} else {
			line := failureLineFailed
			if contextErr != nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				line = failureLineTimedOut
			} else if runErr == nil {
				line = failureLineNoAnswer
			}
			message = o.failConversationLocked(job, attempt.Error, line, now)
		}
	}
	job.QueuePosition = 0
	job.UpdatedAt = now
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	jobCopy = &copy
	o.mu.Unlock()

	o.setActivity(launch.participant, chat.SchedulerWaiting, "provider call complete", "", "conversation responder", chat.OperationWriting, "", "", "provider_call_completed", jobCopy.Deadline)
	switch jobCopy.State {
	case chat.ConversationAnswered:
		o.setActivity(launch.participant, chat.SchedulerDone, "answer delivered", "", "conversation responder", chat.OperationWriting, "", "", "answer_delivered", nil)
	case chat.ConversationFinding, chat.ConversationWaiting:
		o.setActivity(launch.participant, chat.SchedulerWaiting, "provider failed; waiting for alternate provider", "", "conversation responder", chat.OperationWaiting, "confirmed provider failure", "alternate provider", "provider_call_completed", jobCopy.Deadline)
	case chat.ConversationNeedsAttention:
		o.setActivity(launch.participant, chat.SchedulerNeedsAttention, "conversation requires a work decision", "", "conversation responder", chat.OperationOther, jobCopy.TerminalReason, "human action", "requires_work", nil)
	case chat.ConversationFailed:
		o.setActivity(launch.participant, chat.SchedulerDone, "conversation failed", "", "conversation responder", chat.OperationOther, "", "", "failed", nil)
	case chat.ConversationDismissed, chat.ConversationCancelled:
		o.setActivity(launch.participant, chat.SchedulerDone, "conversation cancelled", "", "conversation responder", chat.OperationOther, "", "", "cancelled", nil)
	}

	if message != nil {
		o.send(Event{Type: EventMessage, Message: message})
	}
	for _, warning := range correctionWarnings {
		o.send(Event{Type: EventWarning, Participant: launch.participant, Text: warning})
	}
	if err := o.saveRoom(); err != nil {
		failedAt := time.Now().UTC()
		o.mu.Lock()
		current := o.conversationLocked(launch.id)
		var failureMessage *chat.Message
		if current != nil && current.State != chat.ConversationFailed {
			failureMessage = o.failConversationLocked(current, "could not persist conversation result: "+err.Error(), failureLineNotDurable, failedAt)
			copy := cloneConversationJobs([]chat.ConversationJob{*current})[0]
			jobCopy = &copy
		}
		o.mu.Unlock()
		if failureMessage != nil {
			o.send(Event{Type: EventMessage, Message: failureMessage})
		}
		if retryErr := o.saveRoom(); retryErr != nil {
			err = fmt.Errorf("%v; retry: %w", err, retryErr)
		}
		o.setActivity(launch.participant, chat.SchedulerDone, "conversation failed", "", "conversation responder", chat.OperationOther, "", "", "persistence_failed", nil)
		o.send(Event{Type: EventError, Participant: launch.participant, Err: fmt.Errorf("save conversation result: %w", err)})
	}
	if availabilityErr != nil && !errors.Is(availabilityErr, context.Canceled) && !errors.Is(availabilityErr, context.DeadlineExceeded) {
		availabilityParticipant := launch.participant
		if launch.temporary {
			availabilityParticipant = launch.participant.Provider()
		}
		o.recordProviderAvailability(availabilityParticipant, availabilityErr)
	}
	failed := runErr != nil || contextErr != nil || jobCopy.State == chat.ConversationFailed || jobCopy.State == chat.ConversationCancelled
	record := o.completeTurnCapture(turnID, launch.participant, "conversation responder", task, startedAt, turnOutcome{participant: launch.participant, response: finalSequence, failed: failed, canceled: contextErr != nil || errors.Is(runErr, context.Canceled)}, capture)
	o.send(Event{Type: EventTurnFinished, TurnID: turnID, Participant: launch.participant, Turn: record})
	o.send(Event{Type: EventConversation, Conversation: jobCopy})
	o.signalConversationScheduler()
	if err := o.ResumeQueued(); err != nil {
		o.send(Event{Type: EventError, Err: fmt.Errorf("resume queued work after conversation: %w", err)})
	}
}

func conversationWindowAttemptCount(job *chat.ConversationJob) int {
	count := 0
	for _, attempt := range job.Attempts {
		if attempt.Window == job.AttemptWindow {
			count++
		}
	}
	return count
}

func lastConversationParticipant(job *chat.ConversationJob) chat.Participant {
	for index := len(job.Attempts) - 1; index >= 0; index-- {
		if job.Attempts[index].Participant.ValidAgent() {
			return job.Attempts[index].Participant
		}
	}
	return job.Assigned
}

func (o *Orchestrator) finishConversationActivity(participant chat.Participant, action, transition string) {
	if !participant.ValidAgent() {
		return
	}
	o.mu.Lock()
	if o.room.Activities[participant].Role != "conversation responder" {
		o.mu.Unlock()
		return
	}
	activity := o.setActivityLocked(participant, chat.SchedulerDone, action, "", "conversation responder", chat.OperationOther, "", "", transition, nil)
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		o.send(Event{Type: EventError, Participant: participant, Err: fmt.Errorf("save conversation activity: %w", err)})
	}
	o.send(Event{Type: EventActivity, Participant: participant, Activity: &activity})
}

func containsSequence(values []uint64, sequence uint64) bool {
	for _, value := range values {
		if value == sequence {
			return true
		}
	}
	return false
}

// Every concise failure line a conversation can add to the transcript. They are
// named because startup recovery must recognize a line it already wrote when
// FailureSequence did not survive, and that check cannot drift from the writers.
const (
	failureLineTimedOut    = "Reply timed out; no answer was added."
	failureLineFailed      = "Reply failed; no answer was added."
	failureLineNoAnswer    = "Reply returned no answer; no answer was added."
	failureLineNotSaved    = "Reply could not be saved; no answer was added."
	failureLineNotStarted  = "Reply could not be started; no answer was added."
	failureLineNotDurable  = "Reply could not be saved reliably."
	failureLineNeedsAccess = "Reply requires additional access; no answer was added."
	failureLineInterrupted = "Reply was interrupted before it finished; no answer was added."
)

// conversationFailureLine reports whether text is one of those lines, so a
// transcript that already carries a conversation's failure notice is never
// given a second one.
func conversationFailureLine(text string) bool {
	switch strings.TrimSpace(text) {
	case failureLineTimedOut, failureLineFailed, failureLineNoAnswer, failureLineNotSaved,
		failureLineNotStarted, failureLineNotDurable, failureLineNeedsAccess, failureLineInterrupted:
		return true
	}
	return false
}

func (o *Orchestrator) failConversationLocked(job *chat.ConversationJob, reason, line string, now time.Time) *chat.Message {
	job.State = chat.ConversationFailed
	job.ActionState = ""
	job.TerminalReason = strings.TrimSpace(reason)
	job.Assigned = ""
	job.QueuePosition = 0
	job.WaitReason = ""
	job.Unread = false
	job.UpdatedAt = now
	o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
	if job.Temporary {
		job.RetireAt = &now
	}
	if job.FailureSequence != 0 || strings.TrimSpace(line) == "" {
		return nil
	}
	message, err := o.appendConversationMessageLocked(chat.System, chat.MessageText, job.ID, line)
	if err != nil {
		if job.TerminalReason == "" {
			job.TerminalReason = err.Error()
		}
		return nil
	}
	job.FailureSequence = message.Sequence
	return &message
}

func (o *Orchestrator) CancelConversation(id string) error {
	now := time.Now().UTC()
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	if job.DerivedInboxCategory() != chat.ConversationInboxWorking {
		o.mu.Unlock()
		return fmt.Errorf("conversation is not active")
	}
	participant := lastConversationParticipant(job)
	if turn := o.activeTurns[job.Assigned]; turn.conversationID == id && turn.cancel != nil {
		turn.cancel()
	}
	job.State = chat.ConversationCancelled
	job.ActionState = ""
	job.TerminalReason = "cancelled by the human"
	job.Unread = false
	job.UpdatedAt = now
	o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
	if job.Temporary {
		job.RetireAt = &now
	}
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.finishConversationActivity(participant, "conversation cancelled", "cancelled")
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
	if job.State != chat.ConversationFailed {
		o.mu.Unlock()
		return fmt.Errorf("conversation is not failed")
	}
	job.State = chat.ConversationFinding
	job.Assigned = ""
	job.AttemptWindow++
	job.StartedAt = &now
	deadline := now.Add(job.Class.TotalBudget())
	job.Deadline = &deadline
	job.RetireAt = nil
	job.TerminalReason = ""
	job.ActionState = ""
	job.FailureSequence = 0
	job.WaitReason = ""
	o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
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
	if job.State != chat.ConversationFailed {
		o.mu.Unlock()
		return fmt.Errorf("conversation is not waiting for direction")
	}
	if job.Deadline == nil || now.Before(*job.Deadline) {
		o.mu.Unlock()
		return fmt.Errorf("keep waiting is available only after the hard class deadline")
	}
	if job.ExtensionCount >= 1 {
		o.mu.Unlock()
		return fmt.Errorf("this conversation already received its one explicit deadline extension")
	}
	job.State = chat.ConversationFinding
	job.Assigned = ""
	job.StartedAt = &now
	deadline := now.Add(job.Class.TotalBudget())
	job.Deadline = &deadline
	job.RetireAt = nil
	job.TerminalReason = ""
	job.ActionState = ""
	job.FailureSequence = 0
	job.WaitReason = ""
	job.ExtensionCount++
	job.AttemptWindow++
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
	return o.DismissConversation(id)
}

func (o *Orchestrator) DismissConversation(id string) error {
	o.mu.Lock()
	job := o.conversationLocked(id)
	if job == nil {
		o.mu.Unlock()
		return fmt.Errorf("conversation %q not found", id)
	}
	switch job.DerivedInboxCategory() {
	case chat.ConversationInboxNewAnswer:
		job.Unread = false
	case chat.ConversationInboxActionNeeded:
		job.State = chat.ConversationDismissed
		job.ActionState = ""
		job.Unread = false
		o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
	default:
		o.mu.Unlock()
		return fmt.Errorf("conversation cannot be dismissed")
	}
	participant := lastConversationParticipant(job)
	job.UpdatedAt = time.Now().UTC()
	copy := cloneConversationJobs([]chat.ConversationJob{*job})[0]
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.finishConversationActivity(participant, "conversation dismissed", "dismissed")
	o.send(Event{Type: EventConversation, Conversation: &copy})
	return nil
}

func (o *Orchestrator) DismissAllConversations() error {
	now := time.Now().UTC()
	var changed []chat.ConversationJob
	participants := make(map[chat.Participant]bool)
	o.mu.Lock()
	for index := range o.room.Conversations {
		job := &o.room.Conversations[index]
		switch job.DerivedInboxCategory() {
		case chat.ConversationInboxNewAnswer:
			job.Unread = false
		case chat.ConversationInboxActionNeeded:
			job.State = chat.ConversationDismissed
			job.ActionState = ""
			job.Unread = false
			o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
		default:
			continue
		}
		job.UpdatedAt = now
		if participant := lastConversationParticipant(job); participant.ValidAgent() {
			participants[participant] = true
		}
		changed = append(changed, cloneConversationJobs([]chat.ConversationJob{*job})[0])
	}
	o.mu.Unlock()
	if len(changed) == 0 {
		return nil
	}
	if err := o.saveRoom(); err != nil {
		return err
	}
	for participant := range participants {
		o.finishConversationActivity(participant, "conversation dismissed", "dismissed")
	}
	for index := range changed {
		copy := changed[index]
		o.send(Event{Type: EventConversation, Conversation: &copy})
	}
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
		job := o.conversationLocked(source.ConversationID)
		if job == nil {
			created := chat.ConversationJob{
				ID: source.ConversationID, SourceSequence: source.Sequence, State: chat.ConversationFinding,
				Class: class, WorkflowMode: source.WorkflowMode.WithDefault(), CreatedAt: now,
			}
			o.room.Conversations = append(o.room.Conversations, created)
			job = &o.room.Conversations[len(o.room.Conversations)-1]
		} else {
			// RequiresWork no longer re-adds its source to PendingRoutes, but a
			// durable record for this ID can still exist — from a legacy room, or
			// from a source the human is routing a second time. Continue that
			// record instead of appending a second one with the same ID.
			// Duplicate IDs make the scheduler update one record while the runner
			// resolves another and stalls.
			job.AttemptWindow++
		}
		job.SourceSequence = source.Sequence
		job.State = chat.ConversationFinding
		job.Class = class
		job.WorkflowMode = source.WorkflowMode.WithDefault()
		job.Assigned = ""
		job.Temporary = false
		job.QueuePosition = 0
		job.AnswerSequence = 0
		job.Unread = false
		job.PromotedSequence = 0
		job.StartedAt = &now
		job.Deadline = &deadline
		job.RetireAt = nil
		job.RetiredAt = nil
		job.TerminalReason = ""
		job.ActionState = ""
		job.FailureSequence = 0
		job.WaitReason = ""
		job.UpdatedAt = now
		job.LastActivityAt = now
		if len(job.Requested) == 0 && source.Target.ValidAgent() {
			job.Requested = []chat.Participant{source.Target}
		}
		if source.Route != nil {
			job.RemoteMessageID = source.Route.MessageID
		}
		o.room.InputResolutions[source.Sequence] = chat.InputResolution{
			SourceSequence: source.Sequence, Intent: chat.InputConversation, ResolvedAt: now,
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
	workflowID, err := store.NewID()
	if err != nil {
		o.mu.Unlock()
		return err
	}
	mode := source.WorkflowMode.WithDefault()
	policy := source.DelegationPolicy
	if !policy.Valid() || policy == chat.DelegationAdaptive {
		policy = resolvedDelegationPolicy(o.room.DelegationPolicy, mode, source.Target.ValidAgent(), delegationDefault)
	}
	waitForProvider := false
	if source.Target.ValidAgent() {
		waitForProvider = !o.participantStartEligibilityLocked(source.Target, time.Now(), eligibilityOptions{}).Eligible
	} else {
		waitForProvider = !o.hasStartableCoreLocked(time.Now())
	}
	if replace {
		o.cancelAllLocked()
		o.room.PendingInputs = nil
	}
	resource := workflowResourceForMode(mode)
	writerBlocked := resource == chat.WorkflowWorkspaceWrite && o.writerWorkflow != ""
	queued := waitForProvider || writerBlocked
	version := uint64(0)
	if queued {
		o.room.PendingInputs = append(o.room.PendingInputs, source.Sequence)
	} else {
		o.version++
		version = o.version
	}
	o.registerWorkflowLocked(workflowID, version, []uint64{source.Sequence}, source.Target, mode, policy, resource)
	record := o.room.Workflows[workflowID]
	if queued {
		record.State = chat.WorkflowWaiting
		if writerBlocked {
			record.WaitReason = "waiting for workspace write lease"
			record.Dependency = o.writerWorkflow
		} else if waitForProvider {
			record.WaitReason = "waiting for provider capacity"
			if source.Target.ValidAgent() {
				record.Dependency = string(source.Target.Provider())
			} else {
				record.Dependency = "active core peer"
			}
		}
		o.room.Workflows[workflowID] = record
	}
	o.room.InputResolutions[source.Sequence] = chat.InputResolution{
		SourceSequence: source.Sequence, WorkflowID: workflowID, Intent: chat.InputWork, ResolvedAt: time.Now().UTC(),
	}
	queueCount := len(o.room.PendingInputs)
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.send(Event{Type: EventConversation, WorkflowID: workflowID, Text: fmt.Sprintf("message %d resolved as Work", source.Sequence)})
	if queued {
		o.send(Event{Type: EventQueueChanged, WorkflowID: workflowID, Queued: queueCount, Text: record.WaitReason})
		return nil
	}
	o.mu.Lock()
	moderator, present, cores, notice, err := o.startWorkflowLocked(version)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	if notice != "" {
		o.send(Event{Type: EventWarning, WorkflowID: workflowID, Text: notice})
	}
	if source.Target.ValidAgent() {
		o.warnUnsupportedAttachments(source.Attachments, []chat.Participant{source.Target})
		go o.runDirectWorkflow(source.Sequence, source.Target, cores, version, mode, policy)
	} else {
		go o.runModeratedWorkflow(source.Sequence, moderator, present, cores, version, "", mode, policy)
	}
	return nil
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
	category := job.DerivedInboxCategory()
	participant := lastConversationParticipant(job)
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
	if category != chat.ConversationInboxActionNeeded {
		return fmt.Errorf("conversation does not require a work decision")
	}
	promotedSequence, err := o.postWorkTrackedWithOptions(source.Text, source.Attachments, nil, source.Target, replace, chat.IntentHigh, workSubmissionOptions{modeOverride: source.WorkflowMode, delegationPolicy: source.DelegationPolicy})
	if err != nil {
		return err
	}
	o.mu.Lock()
	job = o.conversationLocked(id)
	if job != nil {
		job.PromotedSequence = promotedSequence
		job.State = chat.ConversationDismissed
		job.ActionState = ""
		job.Unread = false
		job.UpdatedAt = time.Now().UTC()
		o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
	}
	o.mu.Unlock()
	if err := o.saveRoom(); err != nil {
		return err
	}
	o.finishConversationActivity(participant, "conversation moved to Work", "promoted")
	return nil
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
	o.send(Event{Type: EventConversation, Text: "routing choice dismissed"})
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
		job.AttemptWindow++
		job.StartedAt = &now
		deadline := now.Add(job.Class.TotalBudget())
		job.Deadline = &deadline
		job.RetireAt = nil
		job.TerminalReason = ""
		job.ActionState = ""
		job.FailureSequence = 0
		job.WaitReason = ""
		o.room.PendingRoutes = removeSequence(o.room.PendingRoutes, job.SourceSequence)
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

func (o *Orchestrator) appendConversationAgentMessageLocked(participant chat.Participant, conversationID, turnID, text string, result agent.TurnResult, seenThrough uint64) (chat.Message, []string, error) {
	message := chat.Message{
		Sequence: o.nextSequence, TurnID: turnID, Author: participant, Kind: chat.MessageText,
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

func (o *Orchestrator) conversationEmitter(ctx context.Context, participant chat.Participant, conversationID, turnID string, capture *turnCapture) func(agent.Event) {
	return func(event agent.Event) {
		if ctx.Err() != nil {
			return
		}
		event.Agent = participant
		if event.Type == agent.EventTool || event.Type == agent.EventStatus || event.Type == agent.EventActivity {
			o.mu.Lock()
			workspace := o.room.Workspace
			o.mu.Unlock()
			event.Text = agent.SanitizeActivitySummary(workspace, event.Text)
			if event.Activity != nil {
				event.Activity.Action = agent.SanitizeActivitySummary(workspace, event.Activity.Action)
				event.Activity.WaitReason = agent.SanitizeActivitySummary(workspace, event.Activity.WaitReason)
				event.Activity.Dependency = agent.SanitizeActivitySummary(workspace, event.Activity.Dependency)
			}
		}
		if event.Type == agent.EventDelta {
			capture.addDelta(event.Text)
		} else if event.Type == agent.EventReset {
			capture.reset()
		} else if event.Type == agent.EventTool {
			capture.addTool(event.Text)
		}
		switch {
		case event.Type == agent.EventActivity && event.Activity != nil:
			o.applyProviderActivity(participant, *event.Activity)
		case event.Type == agent.EventTool && strings.TrimSpace(event.Text) != "":
			o.applyProviderActivity(participant, agent.ActivityFromText("", event.Text))
		case event.Type == agent.EventDelta:
			o.applyProviderActivity(participant, agent.ActivityEvent{State: chat.SchedulerActive, Action: "writing response", Operation: chat.OperationWriting, Transition: "response_delta"})
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
		o.send(Event{Type: EventAgent, TurnID: turnID, Participant: participant, AgentEvent: &event})
	}
}

func (o *Orchestrator) appendConversationMessageLocked(author chat.Participant, kind chat.MessageKind, conversationID, text string) (chat.Message, error) {
	message := chat.Message{Sequence: o.nextSequence, Author: author, Kind: kind, ConversationID: conversationID, Text: strings.TrimSpace(text), CreatedAt: time.Now().UTC()}
	if turn, ok := o.activeTurns[author]; ok {
		message.TurnID = turn.turnID
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
