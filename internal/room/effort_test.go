package room

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

func TestChatGPTEffortRestoresStandingSettingsAndRetryIdentity(t *testing.T) {
	for _, baseline := range []string{"max", ""} {
		t.Run("baseline_"+baseline, func(t *testing.T) {
			o, peer, _ := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			if err := o.SetAgentSettings(chat.Codex, chat.AgentSettings{Effort: baseline, Permissions: chat.PermissionWorkspace}, false); err != nil {
				t.Fatal(err)
			}
			selection := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}, Reason: "Mechanical edit"}
			route := chat.RouteMetadata{MessageID: "effort-work"}
			message, created, err := o.RequestChatGPTWork("Make the small authorized change", chat.Codex, 0, route, selection)
			if err != nil || !created {
				t.Fatalf("submit: %v", err)
			}
			o.wg.Wait()
			state, _ := o.Snapshot()
			status := state.Workflows[message.WorkflowID].EffortStatus[chat.Codex]
			if peer.request(0).Settings.Effort != "low" || state.Settings[chat.Codex].Effort != baseline || status.RequestedEffort != "low" || status.AppliedEffort != "low" || status.ReportedEffort != "" {
				t.Fatalf("override/baseline/confirmation mismatch: %+v %+v", state.Settings, status)
			}
			if _, created, err := o.RequestChatGPTWork(message.Text, chat.Codex, 0, route, selection); err != nil || created || peer.callCount() != 1 {
				t.Fatal("retry dispatched again", err)
			}
			for _, changed := range []chat.EffortSelection{{Efforts: map[chat.Participant]string{chat.Codex: "high"}, Reason: selection.Reason}, {Efforts: selection.Efforts, Reason: "Different reason"}} {
				if _, _, err := o.RequestChatGPTWork(message.Text, chat.Codex, 0, route, changed); err == nil {
					t.Fatal("operation id accepted a changed effort selection")
				}
			}
			selection.Efforts[chat.Codex] = "high"
			state, _ = o.Snapshot()
			if state.Workflows[message.WorkflowID].EffortSelection.Efforts[chat.Codex] != "low" {
				t.Fatal("caller mutated accepted settings")
			}
			if _, _, err := o.RequestChatGPTWork("Perform an unrelated authorized task", chat.Codex, 0, chat.RouteMetadata{MessageID: "next-work"}); err != nil {
				t.Fatal(err)
			}
			o.wg.Wait()
			if peer.request(1).Settings.Effort != baseline {
				t.Fatalf("override leaked: %+v", peer.request(1).Settings)
			}
		})
	}
}

func TestChatGPTEffortsAreIndependentAcrossConcurrentRepliesAndRound(t *testing.T) {
	o, peers := newFourAgentOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	started, release := make(chan chat.Participant, 2), make(chan struct{})
	for _, participant := range []chat.Participant{chat.Codex, chat.Claude} {
		participant := participant
		peers[participant].run = func(ctx context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			started <- participant
			select {
			case <-release:
			case <-ctx.Done():
			}
			return agent.TurnResult{Text: "independent finding", Done: true, RuntimeEffort: request.Settings.Effort, RuntimeSource: "test provider"}, nil
		}
	}
	selection := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low", chat.Claude: "medium"}}
	message, _, err := o.PublishChatGPT("Inspect independently", 0, []chat.Participant{chat.Codex, chat.Claude}, chat.RouteMetadata{MessageID: "effort-replies"}, selection)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("independent replies did not overlap")
		}
	}
	close(release)
	waitChatGPTReplies(t, o, 2)
	o.wg.Wait()
	state, _ := o.Snapshot()
	for _, job := range state.Conversations {
		status := job.Attempts[0].Effort
		if job.SourceSequence != message.Sequence || status.RequestedEffort != selection.Efforts[job.Assigned] || status.ReportedEffort != status.RequestedEffort {
			t.Fatalf("reply effort: %+v", job)
		}
	}
	var order []chat.Participant
	for participant, peer := range peers {
		participant := participant
		peer.run = func(_ context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
			order = append(order, participant)
			return agent.TurnResult{Text: "review complete", Done: true}, nil
		}
	}
	selection = chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "high", chat.Claude: "low", chat.Agy: "medium", chat.Copilot: "high"}}
	message, _, err = o.RequestChatGPTRound("Review the existing proposal", []chat.Participant{chat.Claude, chat.Agy, chat.Copilot}, 0, chat.RouteMetadata{MessageID: "effort-round"}, selection)
	if err != nil {
		t.Fatal(err)
	}
	o.wg.Wait()
	if !slices.Equal(order, []chat.Participant{chat.Claude, chat.Agy, chat.Copilot, chat.Codex}) {
		t.Fatal("round order", order)
	}
	for participant, peer := range peers {
		request := peer.request(peer.callCount() - 1)
		if request.Settings.Effort != selection.Efforts[participant] || request.Settings.Permissions != chat.PermissionReadOnly {
			t.Fatalf("round effort or permission: %s %+v", participant, request.Settings)
		}
	}
	if _, created, err := o.RequestChatGPTRound(message.Text, []chat.Participant{chat.Claude, chat.Agy, chat.Copilot}, 0, *message.Route, selection); err != nil || created {
		t.Fatal("round retry", err)
	}
}

func TestInvalidEffortDoesNotPostOrDispatch(t *testing.T) {
	o, peer, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	o.mu.Lock()
	o.cacheEffortCatalogLocked(chat.Codex, []agent.ModelOption{{ID: "test-model", Default: true, Efforts: []string{"low", "high"}, EffortsKnown: true}})
	o.mu.Unlock()
	for _, choice := range []chat.EffortSelection{
		{Efforts: map[chat.Participant]string{chat.Codex: "max"}},
		{Efforts: map[chat.Participant]string{chat.Claude: "low"}},
		{Efforts: map[chat.Participant]string{chat.Codex: "not-a-level"}},
		{Efforts: map[chat.Participant]string{chat.Codex: ""}},
		{Reason: "reason without a choice"},
		{Efforts: map[chat.Participant]string{chat.Codex: "low"}, Reason: strings.Repeat("x", 513)},
	} {
		_, _, err := o.RequestChatGPTWork("Bounded task", chat.Codex, 0, chat.RouteMetadata{MessageID: "invalid"}, choice)
		var effortErr *chat.EffortError
		if !errors.As(err, &effortErr) {
			t.Fatalf("accepted invalid choice %+v: %v", choice, err)
		}
	}
	choice := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}}
	if _, _, err := o.PublishChatGPT("Just text", 0, nil, chat.RouteMetadata{MessageID: "text-effort"}, choice); err == nil {
		t.Fatal("text-only effort accepted")
	}
	_, messages := o.Snapshot()
	if len(messages) != 0 || peer.callCount() != 0 {
		t.Fatal("invalid effort produced side effects")
	}
}

func TestQueuedEffortRevalidatesModelAndDoesNotEscalate(t *testing.T) {
	o, codex, claude := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	started, release := make(chan struct{}), make(chan struct{})
	claude.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return agent.TurnResult{Text: "Existing work complete", Done: true}, nil
	}
	if err := o.Post("@claude Implement the existing work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}
	choice := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}}
	message, _, err := o.RequestChatGPTWork("Do the queued task", chat.Codex, 0, chat.RouteMetadata{MessageID: "queued-effort"}, choice)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := o.Snapshot()
	encoded, _ := json.Marshal(state)
	var saved chat.Room
	if err := json.Unmarshal(encoded, &saved); err != nil || saved.Workflows[message.WorkflowID].EffortSelection.Efforts[chat.Codex] != "low" {
		t.Fatal("effort lost in persistence", err)
	}
	o.mu.Lock()
	settings := o.settings[chat.Codex]
	settings.Model = "high-only"
	o.settings[chat.Codex] = settings
	o.cacheEffortCatalogLocked(chat.Codex, []agent.ModelOption{{ID: "high-only", Efforts: []string{"high"}, EffortsKnown: true}})
	o.mu.Unlock()
	close(release)
	o.wg.Wait()
	state, _ = o.Snapshot()
	record := state.Workflows[message.WorkflowID]
	if codex.callCount() != 0 || record.State != chat.WorkflowCancelled || record.EffortStatus[chat.Codex].Error == "" {
		t.Fatalf("incompatible queued effort dispatched: %+v", record)
	}
	if len(state.Availability) != 0 {
		t.Fatal("local validation marked provider unavailable")
	}
}

func TestQueuedChatGPTEffortSurvivesHostRestart(t *testing.T) {
	roomStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	blocking := &fakeAgent{participant: chat.Codex, run: func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}}
	first, err := New(state, nil, roomStore, blocking)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	connectChatGPT(first)
	if err := first.SetAgentSettings(chat.Codex, chat.AgentSettings{Effort: "max", Permissions: chat.PermissionWorkspace}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.RequestChatGPTWork("Implement the existing task", chat.Codex, 0, chat.RouteMetadata{MessageID: "active-effort"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("initial work did not start")
	}
	choice := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}, Reason: "Durable small task"}
	message, _, err := first.RequestChatGPTWork("durable queued task", chat.Codex, 0, chat.RouteMetadata{MessageID: "persisted-effort"}, choice)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := roomStore.LoadRoom(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := roomStore.LoadMessages(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(chan agent.TurnRequest, 1)
	peer := &fakeAgent{participant: chat.Codex, run: func(_ context.Context, _ int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		resumed <- request
		return agent.TurnResult{Text: "resumed result", Done: true}, nil
	}}
	restarted, err := New(loaded, messages, roomStore, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Configure(nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-resumed:
		if request.Settings.Effort != "low" || !strings.Contains(request.Prompt, "durable queued task") {
			t.Fatalf("resumed request: %+v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued effort did not resume")
	}
	restarted.wg.Wait()
	finished, _ := restarted.Snapshot()
	record := finished.Workflows[message.WorkflowID]
	if !record.EffortSelection.Equal(choice) || record.EffortStatus[chat.Codex].AppliedEffort != "low" || finished.Settings[chat.Codex].Effort != "max" {
		t.Fatalf("lost durable effort: %+v", record)
	}
}

func TestProviderDefaultEffortFailureRemainsVisibleWithoutFallback(t *testing.T) {
	for _, kind := range []string{"work", "reply"} {
		t.Run(kind, func(t *testing.T) {
			o, codex, claude := newTestOrchestrator(t)
			defer o.Close()
			connectChatGPT(o)
			codex.run = func(context.Context, int, agent.TurnRequest, func(agent.Event)) (agent.TurnResult, error) {
				return agent.TurnResult{}, &chat.EffortError{Message: "Could not resolve provider default; no turn started"}
			}
			choice := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "auto"}}
			var status chat.EffortStatus
			if kind == "work" {
				message, _, err := o.RequestChatGPTWork("bounded task", chat.Codex, 0, chat.RouteMetadata{MessageID: "default-failure"}, choice)
				if err != nil {
					t.Fatal(err)
				}
				o.wg.Wait()
				state, _ := o.Snapshot()
				status = state.Workflows[message.WorkflowID].EffortStatus[chat.Codex]
			} else {
				_, _, err := o.PublishChatGPT("bounded review", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "default-failure"}, choice)
				if err != nil {
					t.Fatal(err)
				}
				jobs := waitChatGPTReplies(t, o, 1)
				if jobs[0].ReasonCode != chat.ReasonEffortUnsupported {
					t.Fatalf("wrong failure reason: %+v", jobs[0])
				}
				status = jobs[0].Attempts[0].Effort
			}
			state, _ := o.Snapshot()
			if status.Error == "" || status.AppliedEffort != "" || status.RequestedEffort != "auto" || claude.callCount() != 0 || len(state.Availability) != 0 {
				t.Fatalf("resolution failure hidden or escalated: %+v", status)
			}
		})
	}
}

type delayedEffortCatalog struct {
	*fakeAgent
	started, release chan struct{}
	once             sync.Once
}

func (a *delayedEffortCatalog) Models(ctx context.Context) ([]agent.ModelOption, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []agent.ModelOption{{ID: "catalog-model", Default: true, Efforts: []string{"low", "high"}, EffortsKnown: true}}, nil
}

func TestEffortDiscoveryNeverBlocksRoomReadsAndCachesAuxiliaryCapabilities(t *testing.T) {
	o, base, _ := newTestOrchestrator(t)
	defer o.Close()
	peer := &delayedEffortCatalog{fakeAgent: base, started: make(chan struct{}), release: make(chan struct{})}
	auxiliary := chat.Participant("codex-1")
	wrapped, err := agent.WithParticipant(peer, auxiliary)
	if err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.agents[chat.Codex], o.agents[auxiliary] = peer, wrapped
	o.agentGates[auxiliary] = &sync.Mutex{}
	o.room.Members[auxiliary] = true
	o.settings[auxiliary] = chat.AgentSettings{Permissions: chat.PermissionReadOnly}
	o.mu.Unlock()
	done := make(chan []chat.EffortCapability, 1)
	go func() { done <- o.EffortCapabilities() }()
	select {
	case capabilities := <-done:
		if capabilities[0].CapabilitySource != "provider_validation" {
			t.Fatal("claimed model-specific knowledge before discovery")
		}
	case <-time.After(time.Second):
		t.Fatal("room read waited for model discovery")
	}
	<-peer.started
	if _, _ = o.Snapshot(); len(o.EffortCapabilities()) != 3 {
		t.Fatal("busy discovery blocked another read")
	}
	close(peer.release)
	o.wg.Wait()
	for _, capability := range o.EffortCapabilities() {
		if capability.Provider == chat.Codex && (capability.CapabilitySource != "model_catalog" || capability.Model != "catalog-model" || !slices.Equal(capability.AvailableEfforts, []string{"auto", "low", "high"})) {
			t.Fatalf("cached auxiliary capability: %+v", capability)
		}
	}
	connectChatGPT(o)
	choice := chat.EffortSelection{Efforts: map[chat.Participant]string{auxiliary: "low"}}
	if _, _, err := o.PublishChatGPT("Inspect this narrow finding", 0, []chat.Participant{auxiliary}, chat.RouteMetadata{MessageID: "aux-effort"}, choice); err != nil {
		t.Fatal(err)
	}
	jobs := waitChatGPTReplies(t, o, 1)
	if base.request(0).Settings.Effort != "low" || jobs[0].Attempts[0].Participant != auxiliary || jobs[0].Attempts[0].Effort.AppliedEffort != "low" {
		t.Fatalf("auxiliary effort lost: %+v", jobs)
	}
}

func TestCancelledReplyRetainsEffortSelection(t *testing.T) {
	o, peer, _ := newTestOrchestrator(t)
	defer o.Close()
	connectChatGPT(o)
	started := make(chan struct{})
	peer.run = func(ctx context.Context, _ int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
		close(started)
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	}
	choice := chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}}
	if _, _, err := o.PublishChatGPT("Inspect this finding", 0, []chat.Participant{chat.Codex}, chat.RouteMetadata{MessageID: "cancel-effort"}, choice); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not start")
	}
	o.EndChatGPTParticipation(chat.ReasonChatGPTLeft)
	o.wg.Wait()
	jobs := waitChatGPTReplies(t, o, 1)
	if jobs[0].State != chat.ConversationCancelled || !jobs[0].EffortSelection.Equal(choice) || jobs[0].Attempts[0].Effort.AppliedEffort != "low" {
		t.Fatalf("cancelled reply lost effort: %+v", jobs)
	}
}

func TestEffortSessionResetPrecedesTranscriptSelection(t *testing.T) {
	o, peer, _ := newTestOrchestrator(t)
	defer o.Close()
	peer.resetConfig = true
	o.mu.Lock()
	o.messages = []chat.Message{{Sequence: 1, Author: chat.User, Kind: chat.MessageText, Text: "Earlier evidence that must survive an effort reset"}}
	o.room.Sessions[chat.Codex] = chat.AgentSession{ID: "old-session", Cursor: 1}
	o.room.Workflows["same-workflow"] = chat.WorkflowRecord{EffortSelection: chat.EffortSelection{Efforts: map[chat.Participant]string{chat.Codex: "low"}}}
	o.mu.Unlock()
	spec := turnSpec{workflowID: "same-workflow", after: 1, through: 1, coreParticipants: []chat.Participant{chat.Codex}}
	if _, err := o.prepareEffortTurn(chat.Codex, spec, peer); err != nil {
		t.Fatal(err)
	}
	request := o.turnRequest(chat.Codex, spec, nil)
	if !strings.Contains(request.Prompt, "Earlier evidence") || request.Settings.Effort != "low" {
		t.Fatal("session reset lost transcript context")
	}
}
