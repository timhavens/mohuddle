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
	return &fakeController{
		room: roomState,
		messages: []chat.Message{{
			ID: "message-1", Sequence: 1, Author: chat.User, Kind: chat.MessageText,
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
	if !strings.Contains(string(data), `"pending_inputs":1`) {
		t.Fatalf("room view omitted pending-input count: %s", data)
	}
	history := service.Handle(context.Background(), session, request(t, "history-1", "history.get", HistoryRequest{Limit: 10}))
	data, err = json.Marshal(history.Response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/secret") || !strings.Contains(string(data), "image.png") {
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
	if value.Payload.Text != "" || value.Payload.Error != "" || value.Payload.Agent == nil || value.Payload.Agent.Text != "" {
		t.Fatalf("remote event leaked host details: %+v", value.Payload)
	}
}

func TestLocalTurnEventIncludesHostWorkAssignment(t *testing.T) {
	value, err := NewEvent("host", "room", room.Event{Type: room.EventTurnStarted, Participant: chat.Codex, Role: "lead", Task: "implement queue", Queued: 2}, true)
	if err != nil {
		t.Fatal(err)
	}
	if value.Payload.Role != "lead" || value.Payload.Task != "implement queue" || value.Payload.Queued != 2 {
		t.Fatalf("local event payload=%+v", value.Payload)
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
