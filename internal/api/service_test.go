package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

type fakeController struct {
	mu       sync.Mutex
	room     chat.Room
	messages []chat.Message
	status   room.StatusSnapshot
	events   chan room.Event
	posts    []string
	asks     []string
	rounds   []string
	commands []string
	routes   []chat.RouteMetadata
}

func newFakeController() *fakeController {
	now := time.Now().UTC()
	roomState := chat.NewRoom("0123456789abcdef01234567", "/secret/workspace", 3, now)
	roomState.WorkflowMode = chat.WorkflowPlan
	return &fakeController{
		room: roomState,
		messages: []chat.Message{{
			ID: "message-1", Sequence: 1, Author: chat.User, Kind: chat.MessageText, WorkflowMode: chat.WorkflowPlan,
			Text: "hello", CreatedAt: now,
			Attachments: []chat.Attachment{{ID: "image-1", Kind: chat.AttachmentImage, Name: "image.png", Path: "/secret/image.png"}},
		}},
		events: make(chan room.Event, 8),
	}
}

func (f *fakeController) Snapshot() (chat.Room, []chat.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.room, append([]chat.Message(nil), f.messages...)
}

func (f *fakeController) CoreStatus() room.CoreStatus {
	return room.CoreStatus{Active: []chat.Participant{chat.Codex}}
}

func (f *fakeController) StatusSnapshot() room.StatusSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeController) WorkflowActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status.WorkflowActive
}

func (f *fakeController) PostExternal(value string, route chat.RouteMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, value)
	f.routes = append(f.routes, route)
	f.messages = append(f.messages, chat.Message{
		ID: "external-message", Sequence: f.messages[len(f.messages)-1].Sequence + 1,
		Author: chat.User, Kind: chat.MessageText, Text: value, Route: &route, CreatedAt: time.Now().UTC(),
	})
	return nil
}

func (f *fakeController) AskExternal(value string, route chat.RouteMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, value)
	f.routes = append(f.routes, route)
	return nil
}

func (f *fakeController) RoundExternal(value string, route chat.RouteMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rounds = append(f.rounds, value)
	f.routes = append(f.routes, route)
	return nil
}

func (f *fakeController) Continue() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "continue")
	return nil
}

func (f *fakeController) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "stop")
}

func (f *fakeController) SetWorkflowMode(mode chat.WorkflowMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.room.WorkflowMode = mode
	f.commands = append(f.commands, "plan."+map[bool]string{true: "on", false: "off"}[mode.PlanOnly()])
	return nil
}

func (f *fakeController) SetDelegationPolicy(policy chat.DelegationPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.room.DelegationPolicy = policy
	f.commands = append(f.commands, "delegation."+string(policy))
	return nil
}

func (f *fakeController) ExecutePendingPlan() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "plan.execute")
	f.room.PendingPlan = nil
	f.room.WorkflowMode = chat.WorkflowExecute
	return nil
}

func (f *fakeController) DeclinePendingPlan() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "plan.decline")
	f.room.PendingPlan = nil
	return nil
}

func (f *fakeController) ApprovePendingDelegation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "delegation.run")
	f.room.PendingDelegation = nil
	return nil
}

func (f *fakeController) DeclinePendingDelegation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "delegation.solo")
	f.room.PendingDelegation = nil
	return nil
}

func (f *fakeController) ResolveInput(_ uint64, _ chat.InputIntent, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "routing.resolve")
	return nil
}

func (f *fakeController) CancelPendingRoute(uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "routing.cancel")
	return nil
}

func (f *fakeController) AcknowledgeConversation(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.ack")
	return nil
}

func (f *fakeController) DismissConversation(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.dismiss")
	return nil
}

func (f *fakeController) DismissAllConversations() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.dismiss_all")
	return nil
}

func (f *fakeController) CancelConversation(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.cancel")
	return nil
}

func (f *fakeController) RetryConversation(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.retry")
	return nil
}

func (f *fakeController) KeepWaitingConversation(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.wait")
	return nil
}

func (f *fakeController) PromoteConversation(string, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.promote")
	return nil
}

func (f *fakeController) FollowUpConversation(string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "conversation.followup")
	return nil
}

func (f *fakeController) SetPresence(participant chat.Participant, present bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, map[bool]string{true: "join", false: "leave"}[present]+":"+string(participant))
	return nil
}

func (f *fakeController) ScheduleRosterAction(action chat.RosterActionType, participant chat.Participant, executeAt time.Time, reason string) (chat.ScheduledRosterAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "roster.schedule:"+string(action)+":"+string(participant))
	record := chat.ScheduledRosterAction{ID: "scheduled-1", Action: action, Participant: participant, ExecuteAt: executeAt, CreatedAt: time.Now().UTC(), AuthorizedBy: chat.User, Reason: reason, Status: chat.RosterActionPending}
	f.room.RosterActions = append(f.room.RosterActions, record)
	return record, nil
}

func (f *fakeController) CancelRosterAction(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, "roster.cancel:"+id)
	return nil
}

func (f *fakeController) SubscribeEvents(int) (<-chan room.Event, func()) {
	return f.events, func() {}
}

func TestServiceAuthenticatesJoinsAndSanitizesViews(t *testing.T) {
	service, controller, session := testService(t, ClientLocal, ScopeObserve, ScopeParticipate, ScopeAdminister)
	controller.room.PendingInputs = []uint64{2}
	controller.status.WorkflowActive = true
	controller.room.PendingRoutes = []uint64{1}
	controller.room.Conversations = []chat.ConversationJob{{ID: "conversation-1", SourceSequence: 1, State: chat.ConversationWaiting, Class: chat.ConversationQuick}}
	controller.messages[0].InputIntent = chat.InputConversation
	controller.messages[0].IntentConfidence = chat.IntentHigh
	controller.messages[0].ConversationID = "conversation-1"
	planContent := "# Pending plan\n\n- Review it"
	controller.room.PendingPlan = &chat.ProposedPlan{
		ID: "plan", SourceMessageID: "message-1", SourceSequence: 1, Author: chat.Codex,
		Content: planContent, SHA256: chat.ProposedPlanHash(planContent), CreatedAt: time.Now().UTC(),
	}
	join := service.Handle(context.Background(), session, request(t, "join-1", "room.join", JoinRoomRequest{RoomID: controller.room.ID}))
	if !join.Response.OK || session.RoomID != controller.room.ID {
		t.Fatalf("join=%+v session=%+v", join.Response, session)
	}
	view := service.Handle(context.Background(), session, Request{Version: Version, ID: "room-1", Type: "room.get", RoomID: controller.room.ID})
	data, err := json.Marshal(view.Response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "workspace") || strings.Contains(string(data), "/secret") {
		t.Fatalf("room view leaked host data: %s", data)
	}
	if !strings.Contains(string(data), `"pending_inputs":1`) || !strings.Contains(string(data), `"workflow_active":true`) || !strings.Contains(string(data), `"pending_routes":[1]`) || !strings.Contains(string(data), `"conversations"`) || !strings.Contains(string(data), `"inbox_category":"working"`) || !strings.Contains(string(data), `"available_actions":["cancel"]`) || !strings.Contains(string(data), `"reply_counts":{"new":0,"working":1,"action_needed":0}`) || !strings.Contains(string(data), `"workflow_mode":"plan"`) || !strings.Contains(string(data), `"pending_plan"`) || !strings.Contains(string(data), `"id":"plan"`) {
		t.Fatalf("room view omitted pending-input count or workflow mode: %s", data)
	}
	history := service.Handle(context.Background(), session, request(t, "history-1", "history.get", HistoryRequest{Limit: 10}))
	data, err = json.Marshal(history.Response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/secret") || !strings.Contains(string(data), "image.png") || !strings.Contains(string(data), `"workflow_mode":"plan"`) || !strings.Contains(string(data), `"input_intent":"conversation"`) || !strings.Contains(string(data), `"conversation_id":"conversation-1"`) {
		t.Fatalf("history view=%s", data)
	}
}

func TestServiceScopesRoutesDeduplicatesAndPreventsLoops(t *testing.T) {
	service, controller, session := testService(t, ClientLocal, ScopeObserve, ScopeParticipate)
	joinSession(t, service, controller, session)
	send := request(t, "send-1", "message.send", SendMessageRequest{Mode: "post", Text: "hello"})
	send.RoomID = controller.room.ID
	send.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, send); !result.Response.OK {
		t.Fatalf("send=%+v", result.Response)
	}
	if len(controller.posts) != 1 || controller.posts[0] != "hello" {
		t.Fatalf("posts=%v", controller.posts)
	}
	if len(controller.routes) != 1 || controller.routes[0].OriginClientID != session.Identity || controller.routes[0].Hops[len(controller.routes[0].Hops)-1] != service.InstanceID() {
		t.Fatalf("routes=%+v", controller.routes)
	}
	duplicate := service.Handle(context.Background(), session, send)
	if duplicate.Response.OK || duplicate.Response.Error.Code != "duplicate_message" {
		t.Fatalf("duplicate=%+v", duplicate.Response)
	}
	loop := request(t, "send-2", "message.send", SendMessageRequest{Mode: "post", Text: "loop"})
	loop.RoomID = controller.room.ID
	loop.Route = validRoute(t, session)
	loop.Route.Hops = []string{service.InstanceID()}
	if result := service.Handle(context.Background(), session, loop); result.Response.OK || result.Response.Error.Code != "routing_loop" {
		t.Fatalf("loop=%+v", result.Response)
	}
	admin := request(t, "admin-1", "command.invoke", InvokeCommandRequest{Command: "join", Participant: chat.Agy})
	admin.RoomID = controller.room.ID
	admin.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, admin); result.Response.OK || result.Response.Error.Code != "forbidden" {
		t.Fatalf("admin=%+v", result.Response)
	}
}

func TestRemoteGuestsAreRestrictedToReadOnlyAskTurns(t *testing.T) {
	service, controller, session := testService(t, ClientPeer, ScopeObserve, ScopeParticipate)
	joinSession(t, service, controller, session)
	for _, mode := range []string{"post", "round"} {
		value := request(t, "send-"+mode, "message.send", SendMessageRequest{Mode: mode, Text: "hello"})
		value.RoomID = controller.room.ID
		value.Route = validRoute(t, session)
		if result := service.Handle(context.Background(), session, value); result.Response.OK || result.Response.Error.Code != "forbidden" {
			t.Fatalf("%s=%+v", mode, result.Response)
		}
	}
	ask := request(t, "send-ask", "message.send", SendMessageRequest{Mode: "ask", Text: "hello"})
	ask.RoomID = controller.room.ID
	ask.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, ask); !result.Response.OK {
		t.Fatalf("ask=%+v", result.Response)
	}
	if len(controller.asks) != 1 {
		t.Fatalf("asks=%v", controller.asks)
	}
}

func TestBridgeSessionsAreHostScopedAndRemainReadOnly(t *testing.T) {
	service, controller, _ := testService(t, ClientLocal, ScopeObserve)
	session, err := service.NewBridgeSession("phone", "browser", []Scope{ScopeObserve, ScopeParticipate})
	if err != nil {
		t.Fatal(err)
	}
	if session.Kind != ClientBridge || session.Identity != "host-instance/device-phone/browser" {
		t.Fatalf("session=%+v", session)
	}
	joinSession(t, service, controller, session)
	for _, mode := range []string{"post", "round"} {
		value := request(t, "bridge-"+mode, "message.send", SendMessageRequest{Mode: mode, Text: "hello"})
		value.RoomID = controller.room.ID
		value.Route = validRoute(t, session)
		if result := service.Handle(context.Background(), session, value); result.Response.OK || result.Response.Error.Code != "forbidden" {
			t.Fatalf("%s=%+v", mode, result.Response)
		}
	}
	ask := request(t, "bridge-ask", "message.send", SendMessageRequest{Mode: "ask", Text: "hello"})
	ask.RoomID = controller.room.ID
	ask.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, ask); !result.Response.OK {
		t.Fatalf("ask=%+v", result.Response)
	}
	stop := request(t, "bridge-stop", "command.invoke", InvokeCommandRequest{Command: "stop"})
	stop.RoomID = controller.room.ID
	stop.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, stop); !result.Response.OK {
		t.Fatalf("stop=%+v", result.Response)
	}
	if len(controller.commands) != 1 || controller.commands[0] != "stop" {
		t.Fatalf("commands=%v", controller.commands)
	}
	continued := request(t, "bridge-continue", "command.invoke", InvokeCommandRequest{Command: "continue"})
	continued.RoomID = controller.room.ID
	continued.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, continued); result.Response.OK || result.Response.Error.Code != "forbidden" {
		t.Fatalf("continue=%+v", result.Response)
	}
	ack := request(t, "bridge-ack", "command.invoke", InvokeCommandRequest{Command: "conversation.ack", ConversationID: "conversation-1"})
	ack.RoomID = controller.room.ID
	ack.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, ack); !result.Response.OK {
		t.Fatalf("conversation ack=%+v", result.Response)
	}
	dismiss := request(t, "bridge-dismiss", "command.invoke", InvokeCommandRequest{Command: "conversation.dismiss", ConversationID: "conversation-1"})
	dismiss.RoomID = controller.room.ID
	dismiss.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, dismiss); !result.Response.OK {
		t.Fatalf("conversation dismiss=%+v", result.Response)
	}
	dismissAll := request(t, "bridge-dismiss-all", "command.invoke", InvokeCommandRequest{Command: "conversation.dismiss_all"})
	dismissAll.RoomID = controller.room.ID
	dismissAll.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, dismissAll); !result.Response.OK {
		t.Fatalf("conversation dismiss-all=%+v", result.Response)
	}
	wait := request(t, "bridge-wait", "command.invoke", InvokeCommandRequest{Command: "conversation.wait", ConversationID: "conversation-1"})
	wait.RoomID = controller.room.ID
	wait.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, wait); !result.Response.OK {
		t.Fatalf("conversation wait=%+v", result.Response)
	}
	promoteDenied := request(t, "bridge-promote-denied", "command.invoke", InvokeCommandRequest{Command: "conversation.promote", ConversationID: "conversation-1"})
	promoteDenied.RoomID = controller.room.ID
	promoteDenied.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, promoteDenied); result.Response.OK || result.Response.Error.Code != "forbidden" {
		t.Fatalf("conversation promote without admin=%+v", result.Response)
	}
	admin, err := service.NewBridgeSession("admin-phone", "browser", []Scope{ScopeObserve, ScopeParticipate, ScopeAdminister})
	if err != nil {
		t.Fatal(err)
	}
	joinSession(t, service, controller, admin)
	promote := request(t, "bridge-promote", "command.invoke", InvokeCommandRequest{Command: "conversation.promote", ConversationID: "conversation-1"})
	promote.RoomID = controller.room.ID
	promote.Route = validRoute(t, admin)
	if result := service.Handle(context.Background(), admin, promote); !result.Response.OK {
		t.Fatalf("admin conversation promote=%+v", result.Response)
	}
	controller.room.PendingPlan = &chat.ProposedPlan{ID: "plan-1"}
	execute := request(t, "bridge-plan-execute", "command.invoke", InvokeCommandRequest{Command: "plan.execute", PlanID: "plan-1"})
	execute.RoomID = controller.room.ID
	execute.Route = validRoute(t, admin)
	if result := service.Handle(context.Background(), admin, execute); !result.Response.OK {
		t.Fatalf("execute=%+v", result.Response)
	}
	if controller.commands[len(controller.commands)-1] != "plan.execute" {
		t.Fatalf("commands=%v", controller.commands)
	}
	delegationAuto := request(t, "bridge-delegation-auto", "command.invoke", InvokeCommandRequest{Command: "delegation.auto"})
	delegationAuto.RoomID = controller.room.ID
	delegationAuto.Route = validRoute(t, admin)
	if result := service.Handle(context.Background(), admin, delegationAuto); !result.Response.OK || controller.room.DelegationPolicy != chat.DelegationAuto {
		t.Fatalf("delegation auto=%+v policy=%q", result.Response, controller.room.DelegationPolicy)
	}
	controller.room.PendingDelegation = &chat.PendingDelegation{ID: "split-1", WorkflowVersion: 1, SourceSequence: 1, Requester: chat.Codex, Tasks: []chat.DelegationTask{{Participant: chat.Claude, Task: "inspect"}}, CreatedAt: time.Now().UTC()}
	runSplit := request(t, "bridge-delegation-run", "command.invoke", InvokeCommandRequest{Command: "delegation.run", DelegationID: "split-1"})
	runSplit.RoomID = controller.room.ID
	runSplit.Route = validRoute(t, admin)
	if result := service.Handle(context.Background(), admin, runSplit); !result.Response.OK || controller.room.PendingDelegation != nil {
		t.Fatalf("delegation run=%+v pending=%+v", result.Response, controller.room.PendingDelegation)
	}
	join := request(t, "bridge-admin-join", "command.invoke", InvokeCommandRequest{Command: "join", Participant: chat.Agy})
	join.RoomID = controller.room.ID
	join.Route = validRoute(t, admin)
	if result := service.Handle(context.Background(), admin, join); result.Response.OK || result.Response.Error.Code != "forbidden" {
		t.Fatalf("bridge admin join=%+v", result.Response)
	}
	observe, err := service.NewBridgeSession("observer", "browser", []Scope{ScopeObserve})
	if err != nil {
		t.Fatal(err)
	}
	joinSession(t, service, controller, observe)
	observeStop := request(t, "observe-stop", "command.invoke", InvokeCommandRequest{Command: "stop"})
	observeStop.RoomID = controller.room.ID
	observeStop.Route = validRoute(t, observe)
	if result := service.Handle(context.Background(), observe, observeStop); result.Response.OK || result.Response.Error.Code != "forbidden" {
		t.Fatalf("observe stop=%+v", result.Response)
	}
}

func TestRemoteHistoryUsesStableHighWaterAndRedactsHostDetails(t *testing.T) {
	service, controller, session := testService(t, ClientBridge, ScopeObserve)
	now := time.Now().UTC()
	controller.messages = append(controller.messages,
		chat.Message{ID: "tool", Sequence: 2, Author: chat.Codex, Kind: chat.MessageTool, Text: "read /secret/file", Route: &chat.RouteMetadata{MessageID: "route-tool"}, CreatedAt: now},
		chat.Message{ID: "status", Sequence: 3, Author: chat.System, Kind: chat.MessageStatus, Text: "Granted access to /secret/root", CreatedAt: now},
		chat.Message{ID: "later", Sequence: 4, Author: chat.Codex, Kind: chat.MessageText, Text: "public", CreatedAt: now},
	)
	joinSession(t, service, controller, session)
	first := service.Handle(context.Background(), session, request(t, "history-first", "history.get", HistoryRequest{Through: 3, Limit: 1}))
	result, ok := first.Response.Result.(HistoryResult)
	if !first.Response.OK || !ok {
		t.Fatalf("first=%+v", first.Response)
	}
	if len(result.Messages) != 1 || !result.HasMore || result.NextAfter != 1 || result.Through != 3 || result.LatestSequence != 4 {
		t.Fatalf("first history=%+v", result)
	}
	second := service.Handle(context.Background(), session, request(t, "history-second", "history.get", HistoryRequest{After: result.NextAfter, Through: result.Through, Limit: 10}))
	page, ok := second.Response.Result.(HistoryResult)
	if !second.Response.OK || !ok || len(page.Messages) != 2 {
		t.Fatalf("second=%+v", second.Response)
	}
	if page.Messages[0].Text != "[tool activity hidden]" || page.Messages[0].Route != nil || page.Messages[1].Text != "[host status hidden]" {
		t.Fatalf("remote history leaked details: %+v", page.Messages)
	}
}

func TestBridgeSubscriptionSanitizesBeforeTransportFanout(t *testing.T) {
	service, controller, _ := testService(t, ClientLocal, ScopeObserve)
	session, err := service.NewBridgeSession("phone", "events", []Scope{ScopeObserve})
	if err != nil {
		t.Fatal(err)
	}
	joinSession(t, service, controller, session)
	stream, cancel, err := service.Subscribe(session, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	controller.events <- room.Event{Type: room.EventMessage, Message: &chat.Message{
		ID: "tool", Sequence: 2, Author: chat.Codex, Kind: chat.MessageTool, Text: "read /secret/file",
	}}
	select {
	case event := <-stream:
		if event.Payload.Message == nil || event.Payload.Message.Text != "[tool activity hidden]" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge event")
	}
}

func TestBridgeSubscriptionPreservesStructuredUpstreamGapWithoutText(t *testing.T) {
	service, controller, _ := testService(t, ClientLocal, ScopeObserve)
	session, err := service.NewBridgeSession("phone", "events", []Scope{ScopeObserve})
	if err != nil {
		t.Fatal(err)
	}
	joinSession(t, service, controller, session)
	stream, cancel, err := service.Subscribe(session, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	controller.events <- room.Event{Type: room.EventWarning, Text: "event stream gap: private detail", StreamGap: 7}
	select {
	case event := <-stream:
		if event.Payload.StreamGap != 7 || event.Payload.Text != "" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge gap event")
	}
}

func TestServiceRebuildsDeduplicationFromTranscriptRoutes(t *testing.T) {
	service, controller, session := testService(t, ClientLocal, ScopeObserve, ScopeParticipate)
	joinSession(t, service, controller, session)
	send := request(t, "send", "message.send", SendMessageRequest{Mode: "post", Text: "persisted"})
	send.RoomID = controller.room.ID
	send.Route = validRoute(t, session)
	if result := service.Handle(context.Background(), session, send); !result.Response.OK {
		t.Fatalf("send=%+v", result.Response)
	}
	restarted, err := NewService(service.credentials, controller)
	if err != nil {
		t.Fatal(err)
	}
	restartedSession, err := restarted.Authenticate(HelloRequest{ClientID: "client", Token: service.credentials.Entries[0].Token})
	if err != nil {
		t.Fatal(err)
	}
	joinSession(t, restarted, controller, restartedSession)
	if result := restarted.Handle(context.Background(), restartedSession, send); result.Response.OK || result.Response.Error.Code != "duplicate_message" {
		t.Fatalf("replayed send=%+v", result.Response)
	}
}

func TestRemoteEventViewSuppressesHostAndToolDetails(t *testing.T) {
	value, err := NewEvent("host", "room", room.Event{
		Type: room.EventAgent, Text: "/secret/warning", Err: context.DeadlineExceeded,
		AgentEvent: &agent.Event{Type: agent.EventTool, Agent: chat.Codex, Text: "read /secret/file"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.Text != "" || value.Payload.Error != "" || value.Payload.Agent == nil || value.Payload.Agent.Text != "" || value.Route.OriginInstanceID != "" {
		t.Fatalf("remote event leaked host details: %+v", value.Payload)
	}
}

func TestRemoteEventViewSuppressesAgentDeltaText(t *testing.T) {
	value, err := NewEvent("host", "room", room.Event{
		Type:       room.EventAgent,
		AgentEvent: &agent.Event{Type: agent.EventDelta, Agent: chat.Codex, Text: "<!-- mohuddle:{secret} --> /private/path"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.Agent == nil || value.Payload.Agent.Text != "" {
		t.Fatalf("remote delta leaked host details: %+v", value.Payload)
	}
}

func TestLocalTurnEventIncludesHostWorkAssignment(t *testing.T) {
	value, err := NewEvent("host", "room", room.Event{Type: room.EventTurnStarted, TurnID: "turn-1", Participant: chat.Codex, Role: "lead", Task: "inspect queue", Queued: 2, WorkflowMode: chat.WorkflowPlan}, true)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.TurnID != "turn-1" || value.Payload.Role != "lead" || value.Payload.Task != "inspect queue" || value.Payload.Queued != 2 || value.Payload.WorkflowMode != chat.WorkflowPlan {
		t.Fatalf("local event payload=%+v", value.Payload)
	}
}

func TestTurnDetailsAreLocalOnlyInEventView(t *testing.T) {
	record := &chat.TurnRecord{ID: "turn-1", Participant: chat.Codex, State: chat.TurnRecordSilent, Drafts: []string{"visible local draft"}, Tools: []string{"local tool"}}
	local, err := NewEvent("host", "room", room.Event{Type: room.EventTurnFinished, TurnID: "turn-1", Participant: chat.Codex, Turn: record}, true)
	if err != nil {
		t.Fatal(err)
	}
	if local.Payload.Turn == nil || local.Payload.Turn.Drafts[0] != "visible local draft" {
		t.Fatalf("local payload=%+v", local.Payload)
	}
	remote, err := NewEvent("host", "room", room.Event{Type: room.EventTurnFinished, TurnID: "turn-1", Participant: chat.Codex, Turn: record}, false)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Payload.Turn != nil || remote.Payload.TurnID != "turn-1" {
		t.Fatalf("remote payload=%+v", remote.Payload)
	}
}

func TestPlanReadyEventIncludesExactProposal(t *testing.T) {
	content := "# Proposed\n\n- Verify it"
	plan := chat.ProposedPlan{
		ID: "plan", SourceMessageID: "source", SourceSequence: 4, Author: chat.Codex,
		Content: content, SHA256: chat.ProposedPlanHash(content), CreatedAt: time.Now().UTC(),
	}
	value, err := NewEvent("host", "room", room.Event{Type: room.EventPlanReady, Participant: chat.Codex, Plan: &plan, Text: "Implement the plan?"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.Plan == nil || !value.Payload.Plan.Valid() || value.Payload.Plan.Content != content || value.Payload.Text != "Implement the plan?" {
		t.Fatalf("plan-ready payload=%+v", value.Payload)
	}
}

func TestConversationEventIncludesDurableLifecycle(t *testing.T) {
	job := chat.ConversationJob{ID: "conversation-1", SourceSequence: 7, State: chat.ConversationAnswering, Class: chat.ConversationQuick, QueuePosition: 2}
	value, err := NewEvent("host", "room", room.Event{Type: room.EventConversation, Conversation: &job}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.Conversation == nil || value.Payload.Conversation.ID != job.ID || value.Payload.Conversation.State != chat.ConversationAnswering || value.Payload.Conversation.QueuePosition != 2 || value.Payload.Conversation.InboxCategory != chat.ConversationInboxWorking || len(value.Payload.Conversation.AvailableActions) != 1 || value.Payload.Conversation.AvailableActions[0] != chat.ConversationActionCancel {
		t.Fatalf("conversation payload=%+v", value.Payload)
	}
}

func TestServiceInvokesNarrowScopedCommandSet(t *testing.T) {
	service, controller, session := testService(t, ClientLocal, ScopeObserve, ScopeParticipate, ScopeAdminister)
	joinSession(t, service, controller, session)
	values := []InvokeCommandRequest{
		{Command: "continue"},
		{Command: "stop"},
		{Command: "join", Participant: chat.Agy},
		{Command: "leave", Participant: chat.Agy},
		{Command: "roster.schedule", Participant: chat.Participant("codex-1"), Action: chat.RosterActionJoin, ExecuteAt: time.Now().Add(time.Hour), Reason: "quota retry"},
		{Command: "roster.cancel", ActionID: "scheduled-1"},
	}
	for index, value := range values {
		request := request(t, fmt.Sprintf("command-%d", index), "command.invoke", value)
		request.RoomID = controller.room.ID
		request.Route = validRoute(t, session)
		if result := service.Handle(context.Background(), session, request); !result.Response.OK {
			t.Fatalf("command %q=%+v", value.Command, result.Response)
		}
	}
	if got := strings.Join(controller.commands, ","); got != "continue,stop,join:agy,leave:agy,roster.schedule:join:codex-1,roster.cancel:scheduled-1" {
		t.Fatalf("commands=%s", got)
	}
}

func testService(t *testing.T, kind ClientKind, scopes ...Scope) (*Service, *fakeController, *Session) {
	t.Helper()
	credential := Credential{ID: "test", Token: strings.Repeat("a", 64), Kind: kind, Scopes: scopes}
	if kind == ClientPeer {
		credential.InstanceID = "peer-instance"
	}
	credentials := Credentials{InstanceID: "host-instance", Entries: []Credential{credential}}
	controller := newFakeController()
	service, err := NewService(credentials, controller)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(HelloRequest{ClientID: "client", Token: credential.Token})
	if err != nil {
		t.Fatal(err)
	}
	return service, controller, session
}

func joinSession(t *testing.T, service *Service, controller *fakeController, session *Session) {
	t.Helper()
	result := service.Handle(context.Background(), session, request(t, "join", "room.join", JoinRoomRequest{RoomID: controller.room.ID}))
	if !result.Response.OK {
		t.Fatalf("join=%+v", result.Response)
	}
}

func request(t *testing.T, id, kind string, payload any) Request {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Version: Version, ID: id, Type: kind, Payload: data}
}

func validRoute(t *testing.T, session *Session) *Route {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return &Route{MessageID: id, OriginInstanceID: session.InstanceID, OriginClientID: session.Identity}
}
