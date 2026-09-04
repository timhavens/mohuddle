package remote

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/remote/device"
	"github.com/timhavens/mohuddle/internal/remote/events"
	"github.com/timhavens/mohuddle/internal/remoteui"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/testutil"
)

type gatewayController struct {
	mu       sync.Mutex
	room     chat.Room
	messages []chat.Message
	asks     []string
	stops    int
	controls []string
	events   map[chan room.Event]struct{}
}

func newGatewayController() *gatewayController {
	now := time.Now().UTC()
	return &gatewayController{
		room:     chat.Room{ID: "remote-test-room", CreatedAt: now, UpdatedAt: now, Members: map[chat.Participant]bool{chat.Codex: true}},
		messages: []chat.Message{{ID: "initial", Sequence: 1, Author: chat.Codex, Kind: chat.MessageText, Text: "hello phone", CreatedAt: now}},
		events:   make(map[chan room.Event]struct{}),
	}
}

func (c *gatewayController) Snapshot() (chat.Room, []chat.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.room, append([]chat.Message(nil), c.messages...)
}

func (c *gatewayController) CoreStatus() room.CoreStatus                    { return room.CoreStatus{} }
func (c *gatewayController) PostExternal(string, chat.RouteMetadata) error  { return nil }
func (c *gatewayController) RoundExternal(string, chat.RouteMetadata) error { return nil }

func (c *gatewayController) AskExternal(text string, route chat.RouteMetadata) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks = append(c.asks, text)
	c.messages = append(c.messages, chat.Message{
		ID: route.MessageID, Sequence: uint64(len(c.messages) + 1), Author: chat.User,
		Kind: chat.MessageText, Text: text, Route: &route, CreatedAt: time.Now().UTC(),
	})
	return nil
}

func (c *gatewayController) Continue() error { return nil }
func (c *gatewayController) Stop() {
	c.mu.Lock()
	c.stops++
	c.mu.Unlock()
}
func (c *gatewayController) SetWorkflowMode(mode chat.WorkflowMode) error {
	c.mu.Lock()
	c.room.WorkflowMode = mode
	c.controls = append(c.controls, "plan."+map[bool]string{true: "on", false: "off"}[mode.PlanOnly()])
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) SetDelegationPolicy(policy chat.DelegationPolicy) error {
	c.mu.Lock()
	c.room.DelegationPolicy = policy
	c.controls = append(c.controls, "delegation."+string(policy))
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) ExecutePendingPlanID(planID string) error {
	c.mu.Lock()
	if c.room.PendingPlan == nil || c.room.PendingPlan.ID != planID {
		c.mu.Unlock()
		return fmt.Errorf("stale plan")
	}
	c.room.PendingPlan = nil
	c.room.WorkflowMode = chat.WorkflowExecute
	c.controls = append(c.controls, "plan.execute")
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) DeclinePendingPlanID(planID string) error {
	c.mu.Lock()
	if c.room.PendingPlan == nil || c.room.PendingPlan.ID != planID {
		c.mu.Unlock()
		return fmt.Errorf("stale plan")
	}
	c.room.PendingPlan = nil
	c.room.WorkflowMode = chat.WorkflowPlan
	c.controls = append(c.controls, "plan.decline")
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) ApprovePendingDelegation() error {
	c.mu.Lock()
	c.room.PendingDelegation = nil
	c.controls = append(c.controls, "delegation.run")
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) DeclinePendingDelegation() error {
	c.mu.Lock()
	c.room.PendingDelegation = nil
	c.controls = append(c.controls, "delegation.solo")
	c.mu.Unlock()
	return nil
}
func (c *gatewayController) ResolveInput(uint64, chat.InputIntent, bool) error { return nil }
func (c *gatewayController) CancelPendingRoute(uint64) error                   { return nil }
func (c *gatewayController) AcknowledgeConversation(string) error              { return nil }
func (c *gatewayController) DismissConversation(string) error                  { return nil }
func (c *gatewayController) DismissAllConversations() error                    { return nil }
func (c *gatewayController) CancelConversation(string) error                   { return nil }
func (c *gatewayController) RetryConversation(string) error                    { return nil }
func (c *gatewayController) KeepWaitingConversation(string) error              { return nil }
func (c *gatewayController) PromoteConversation(string, bool) error            { return nil }
func (c *gatewayController) FollowUpConversation(string, string) error         { return nil }
func (c *gatewayController) SetPresence(chat.Participant, bool) error          { return nil }
func (c *gatewayController) ScheduleRosterAction(chat.RosterActionType, chat.Participant, time.Time, string) (chat.ScheduledRosterAction, error) {
	return chat.ScheduledRosterAction{}, nil
}
func (c *gatewayController) CancelRosterAction(string) error { return nil }

func (c *gatewayController) SubscribeEvents(_ int) (<-chan room.Event, func()) {
	c.mu.Lock()
	values := make(chan room.Event, 32)
	c.events[values] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return values, func() {
		once.Do(func() {
			c.mu.Lock()
			if _, ok := c.events[values]; ok {
				delete(c.events, values)
				close(values)
			}
			c.mu.Unlock()
		})
	}
}

func (c *gatewayController) emit(event room.Event) {
	c.mu.Lock()
	streams := make([]chan room.Event, 0, len(c.events))
	for stream := range c.events {
		streams = append(streams, stream)
	}
	c.mu.Unlock()
	for _, stream := range streams {
		stream <- event
	}
}

func TestGatewayRejectsUnsafeListenerConfiguration(t *testing.T) {
	_, err := Start(Config{ListenAddress: "0.0.0.0:0"})
	if err == nil || err.Error() != "remote gateway service, device store, and assets are required" {
		t.Fatalf("missing dependencies: %v", err)
	}
	controller := newGatewayController()
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = Start(Config{
		ListenAddress: "0.0.0.0:0", RoomID: controller.room.ID,
		Service: service, Devices: store, Assets: remoteui.FS(),
	})
	if err == nil || err.Error() != "non-loopback remote access requires TLS" {
		t.Fatalf("unsafe listener: %v", err)
	}
	_, err = Start(Config{
		ListenAddress: "127.0.0.1:0", TLSCertFile: filepath.Join(t.TempDir(), "missing-cert"), TLSKeyFile: filepath.Join(t.TempDir(), "missing-key"),
		RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS(),
	})
	if err == nil || !strings.Contains(err.Error(), "load remote TLS identity") {
		t.Fatalf("invalid TLS identity error=%v", err)
	}
}

func TestGatewayPairAuthenticateAskReconnectAndRevoke(t *testing.T) {
	controller := newGatewayController()
	service := gatewayService(t, controller)
	stateDir := testDeviceStateDir(t)
	store, err := device.Open(filepath.Join(stateDir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{
		ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID,
		Service: service, Devices: store, Audit: api.NewAuditLog(filepath.Join(stateDir, "audit.jsonl")),
		Assets: remoteui.FS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()

	invitation, err := store.CreateInvitation(controller.room.ID, "test phone", []device.Scope{device.ScopeObserve, device.ScopeParticipate}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	pairResponse := postGateway(t, client, gateway.Origin(), "/api/v1/pair", pairRequest{
		Code: invitation.Code, Name: "ignored client label", PublicKey: base64.StdEncoding.EncodeToString(publicKey),
	}, "", "")
	if pairResponse.StatusCode != http.StatusOK {
		t.Fatalf("pair status=%d body=%s", pairResponse.StatusCode, readBody(t, pairResponse))
	}
	var paired pairResult
	decodeResponse(t, pairResponse, &paired)
	if paired.DeviceID == "" || paired.Name != "test phone" || paired.PermissionCeiling != "read-only" {
		t.Fatalf("pair=%+v", paired)
	}

	challengeResponse := postGateway(t, client, gateway.Origin(), "/api/v1/challenge", challengeRequest{DeviceID: paired.DeviceID}, "", "")
	var challenge challengeResult
	decodeResponse(t, challengeResponse, &challenge)
	signature := signRawP256(t, privateKey, []byte(challenge.Payload))
	sessionResponse := postGateway(t, client, gateway.Origin(), "/api/v1/session", sessionRequest{
		DeviceID: paired.DeviceID, ChallengeID: challenge.ChallengeID,
		Signature: base64.StdEncoding.EncodeToString(signature),
	}, "", "")
	if sessionResponse.StatusCode != http.StatusOK {
		t.Fatalf("session status=%d body=%s", sessionResponse.StatusCode, readBody(t, sessionResponse))
	}
	var session sessionResult
	cookies := sessionResponse.Cookies()
	decodeResponse(t, sessionResponse, &session)
	if len(cookies) != 1 || session.CSRFToken == "" || session.Identity == "" {
		t.Fatalf("session=%+v cookies=%d", session, len(cookies))
	}

	joinPayload, _ := json.Marshal(api.JoinRoomRequest{RoomID: controller.room.ID})
	joinRequest := api.Request{Version: api.Version, ID: "phone-join", Type: "room.join", Payload: joinPayload}
	response := postGateway(t, client, gateway.Origin(), "/api/v1/request", joinRequest, cookies[0].String(), session.CSRFToken)
	var joinResponse api.Response
	decodeResponse(t, response, &joinResponse)
	if !joinResponse.OK {
		t.Fatalf("join response=%+v", joinResponse)
	}

	request := api.Request{Version: api.Version, ID: "phone-room", Type: "room.get", RoomID: "spoofed"}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", request, cookies[0].String(), session.CSRFToken)
	var roomResponse api.Response
	decodeResponse(t, response, &roomResponse)
	if !roomResponse.OK {
		t.Fatalf("room response=%+v", roomResponse)
	}

	payload, _ := json.Marshal(api.SendMessageRequest{Mode: "ask", Text: "inspect safely"})
	request = api.Request{Version: api.Version, ID: "phone-ask", Type: "message.send", Route: &api.Route{MessageID: "spoofed"}, Payload: payload}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", request, cookies[0].String(), session.CSRFToken)
	var askResponse api.Response
	decodeResponse(t, response, &askResponse)
	if !askResponse.OK {
		t.Fatalf("ask response=%+v", askResponse)
	}
	controller.mu.Lock()
	asks := append([]string(nil), controller.asks...)
	controller.mu.Unlock()
	if len(asks) != 1 || asks[0] != "inspect safely" {
		t.Fatalf("asks=%v", asks)
	}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", request, cookies[0].String(), session.CSRFToken)
	var duplicateResponse api.Response
	decodeResponse(t, response, &duplicateResponse)
	controller.mu.Lock()
	duplicateAsks := append([]string(nil), controller.asks...)
	controller.mu.Unlock()
	if duplicateResponse.OK || duplicateResponse.Error == nil || duplicateResponse.Error.Code != "duplicate_message" || len(duplicateAsks) != 1 {
		t.Fatalf("duplicate response=%+v asks=%v", duplicateResponse, duplicateAsks)
	}

	stopPayload := json.RawMessage(`{"command":"stop"}`)
	stopRequest := api.Request{Version: api.Version, ID: "phone-stop", Type: "command.invoke", Payload: stopPayload}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", stopRequest, cookies[0].String(), session.CSRFToken)
	var stopResponse api.Response
	decodeResponse(t, response, &stopResponse)
	controller.mu.Lock()
	stops := controller.stops
	controller.mu.Unlock()
	if !stopResponse.OK || stops != 1 {
		t.Fatalf("stop response=%+v stops=%d", stopResponse, stops)
	}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", stopRequest, cookies[0].String(), session.CSRFToken)
	var duplicateStop api.Response
	decodeResponse(t, response, &duplicateStop)
	controller.mu.Lock()
	stops = controller.stops
	controller.mu.Unlock()
	if duplicateStop.OK || duplicateStop.Error == nil || duplicateStop.Error.Code != "duplicate_message" || stops != 1 {
		t.Fatalf("duplicate stop=%+v stops=%d", duplicateStop, stops)
	}
	continuePayload, _ := json.Marshal(api.InvokeCommandRequest{Command: "continue"})
	continueRequest := api.Request{Version: api.Version, ID: "phone-continue", Type: "command.invoke", Payload: continuePayload}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", continueRequest, cookies[0].String(), session.CSRFToken)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("continue status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()

	wsOrigin := "ws://" + gateway.Addr()
	streamURL := wsOrigin + "/api/v1/events?room_id=" + url.QueryEscape(controller.room.ID) + "&after_event=0&after_message=0"
	headers := http.Header{"Origin": []string{gateway.Origin()}, "Cookie": []string{cookies[0].String()}}
	connection, _, err := websocket.Dial(context.Background(), streamURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	var sync syncFrame
	if err := wsjson.Read(context.Background(), connection, &sync); err != nil {
		t.Fatal(err)
	}
	if sync.Type != "sync" || sync.Room.ID != controller.room.ID || sync.Cursor.MessageSequence < 1 || len(sync.History.Messages) < 1 {
		t.Fatalf("sync=%+v", sync)
	}

	if err := store.Revoke(paired.DeviceID); err != nil {
		t.Fatal(err)
	}
	readContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var frame any
	if err := wsjson.Read(readContext, connection, &frame); err == nil {
		t.Fatal("revoked stream remained open")
	}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", request, cookies[0].String(), session.CSRFToken)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked request status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()

	records, err := api.NewAuditLog(filepath.Join(stateDir, "audit.jsonl")).Recent(50)
	if err != nil {
		t.Fatal(err)
	}
	foundPair, foundAsk, foundStop := false, false, false
	for _, record := range records {
		if record.Action == "remote.pair" && record.DeviceID == paired.DeviceID && record.Allowed {
			foundPair = true
		}
		if record.Action == "message.send" && record.DeviceID == paired.DeviceID && record.Permission == "read-only" && record.Allowed {
			foundAsk = true
		}
		if record.Action == "command.stop" && record.DeviceID == paired.DeviceID && record.Permission == "read-only" && record.Allowed {
			foundStop = true
		}
	}
	if !foundPair || !foundAsk || !foundStop {
		t.Fatalf("audit pair=%v ask=%v stop=%v records=%+v", foundPair, foundAsk, foundStop, records)
	}
}

func TestGatewayRequiresExactOriginAndCSRF(t *testing.T) {
	controller := newGatewayController()
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS()})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	request, _ := http.NewRequest(http.MethodPost, gateway.Origin()+"/api/v1/challenge", bytes.NewReader([]byte(`{"device_id":"none"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://attacker.invalid")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("origin status=%d", response.StatusCode)
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("security headers=%v", response.Header)
	}
}

func TestRemoteRequestActionAllowsOnlyNarrowControlCommands(t *testing.T) {
	stop := json.RawMessage(`{"command":" STOP "}`)
	if action, ok := remoteRequestAction(api.Request{Type: "command.invoke", Payload: stop}); !ok || action != "command.stop" {
		t.Fatalf("stop action=%q allowed=%v", action, ok)
	}
	for name, request := range map[string]api.Request{
		"smuggled participant":      {Type: "command.invoke", Payload: json.RawMessage(`{"command":"stop","participant":"codex"}`)},
		"smuggled reason":           {Type: "command.invoke", Payload: json.RawMessage(`{"command":"stop","reason":"extra"}`)},
		"unknown field":             {Type: "command.invoke", Payload: json.RawMessage(`{"command":"stop","extra":true}`)},
		"smuggled conversation":     {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.ack","conversation_id":"one","extra":true}`)},
		"work route missing intent": {Type: "command.invoke", Payload: json.RawMessage(`{"command":"routing.resolve","sequence":4}`)},
		"malformed":                 {Type: "command.invoke", Payload: json.RawMessage(`{"command":`)},
		"unexposed":                 {Type: "plan.execute"},
	} {
		t.Run(name, func(t *testing.T) {
			if action, ok := remoteRequestAction(request); ok || action != "" {
				t.Fatalf("action=%q allowed=%v", action, ok)
			}
		})
	}
	for name, request := range map[string]api.Request{
		"continue":    {Type: "command.invoke", Payload: json.RawMessage(`{"command":"continue"}`)},
		"plan on":     {Type: "command.invoke", Payload: json.RawMessage(`{"command":"plan.on"}`)},
		"execute":     {Type: "command.invoke", Payload: json.RawMessage(`{"command":"plan.execute","plan_id":"plan-1"}`)},
		"ack":         {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.ack","conversation_id":"one"}`)},
		"dismiss":     {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.dismiss","conversation_id":"one"}`)},
		"dismiss all": {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.dismiss_all"}`)},
		"wait":        {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.wait","conversation_id":"one"}`)},
		"followup":    {Type: "command.invoke", Payload: json.RawMessage(`{"command":"conversation.followup","conversation_id":"one","text":"why?"}`)},
		"route chat":  {Type: "command.invoke", Payload: json.RawMessage(`{"command":"routing.resolve","sequence":4,"intent":"conversation"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if action, ok := remoteRequestAction(request); !ok || action == "" {
				t.Fatalf("action=%q allowed=%v", action, ok)
			}
		})
	}
}

func TestRemoteFramesExposeWorkflowSnapshotsWithoutAddingCommands(t *testing.T) {
	now := time.Now().UTC()
	frame := syncFrame{
		Type:    "sync",
		History: api.HistoryResult{Messages: []api.MessageView{{ID: "message-1", Sequence: 1, WorkflowID: "workflow-1", Author: chat.Codex, Kind: chat.MessageText, Text: "done", CreatedAt: now}}},
		Room: api.RoomView{
			ID: "room", CreatedAt: now, UpdatedAt: now, Members: map[chat.Participant]bool{chat.Codex: true},
			Workflows:        map[string]chat.WorkflowRecord{"workflow-1": {ID: "workflow-1", Generation: 1, SourceSequences: []uint64{1}, Mode: chat.WorkflowExecute, DelegationPolicy: chat.DelegationAuto, Resource: chat.WorkflowReadOnly, State: chat.WorkflowActive, CreatedAt: now, UpdatedAt: now}},
			InputResolutions: map[uint64]chat.InputResolution{1: {SourceSequence: 1, WorkflowID: "workflow-1", Intent: chat.InputWork, ResolvedAt: now}},
		},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decoded syncFrame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.History.Messages[0].WorkflowID != "workflow-1" || decoded.Room.Workflows["workflow-1"].ID != "workflow-1" || decoded.Room.InputResolutions[1].WorkflowID != "workflow-1" {
		t.Fatalf("decoded workflow frame=%+v", decoded)
	}

	action, allowed := remoteRequestAction(api.Request{Type: "command.invoke", Payload: json.RawMessage(`{"command":"stop","workflow_id":"workflow-1"}`)})
	if allowed || action != "" {
		t.Fatalf("workflow-targeted command exposed early: action=%q allowed=%v", action, allowed)
	}
}

func TestAdminPhoneMayApproveExactPlanButCannotRunGeneralAdminCommands(t *testing.T) {
	controller := newGatewayController()
	controller.room.WorkflowMode = chat.WorkflowPlan
	controller.room.PendingPlan = &chat.ProposedPlan{ID: "plan-1", Content: "# Exact plan"}
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS()})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	auth := authenticateTestDevice(t, client, gateway, store, controller.room.ID, []device.Scope{device.ScopeObserve, device.ScopeParticipate, device.ScopeAdmin})

	execute := api.Request{Version: api.Version, ID: "execute-plan", Type: "command.invoke", Payload: json.RawMessage(`{"command":"plan.execute","plan_id":"plan-1"}`)}
	response := postGateway(t, client, gateway.Origin(), "/api/v1/request", execute, auth.Cookie, auth.CSRFToken)
	var result api.Response
	decodeResponse(t, response, &result)
	if !result.OK {
		t.Fatalf("execute=%+v", result)
	}
	controller.mu.Lock()
	controls := append([]string(nil), controller.controls...)
	controller.mu.Unlock()
	if len(controls) != 1 || controls[0] != "plan.execute" {
		t.Fatalf("controls=%v", controls)
	}

	stale := api.Request{Version: api.Version, ID: "stale-plan", Type: "command.invoke", Payload: json.RawMessage(`{"command":"plan.execute","plan_id":"plan-1"}`)}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", stale, auth.Cookie, auth.CSRFToken)
	decodeResponse(t, response, &result)
	if result.OK || result.Error == nil || result.Error.Code != "stale_plan" {
		t.Fatalf("stale=%+v", result)
	}

	join := api.Request{Version: api.Version, ID: "remote-join", Type: "command.invoke", Payload: json.RawMessage(`{"command":"join","participant":"agy"}`)}
	response = postGateway(t, client, gateway.Origin(), "/api/v1/request", join, auth.Cookie, auth.CSRFToken)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("join status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
}

func TestGatewayReplaySyncAcknowledgesOnlyDeliveredCursor(t *testing.T) {
	controller := newGatewayController()
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS()})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	authenticated := authenticateTestDevice(t, client, gateway, store, controller.room.ID, []device.Scope{device.ScopeObserve})

	first, err := gateway.hub.Publish(api.Event{Version: api.Version, ID: "replay-one", Type: "event", RoomID: controller.room.ID, Payload: api.EventPayload{Type: "warning"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.hub.Publish(api.Event{Version: api.Version, ID: "replay-two", Type: "event", RoomID: controller.room.ID, Payload: api.EventPayload{Type: "warning"}}); err != nil {
		t.Fatal(err)
	}
	after := events.Cursor{BootID: first.BootID, EventSequence: 0, MessageSequence: 1}
	connection := dialEvents(t, gateway, controller.room.ID, authenticated.Cookie, after)
	defer connection.CloseNow()
	var sync syncFrame
	if err := wsjson.Read(context.Background(), connection, &sync); err != nil {
		t.Fatal(err)
	}
	if sync.Cursor != after {
		t.Fatalf("sync cursor advanced before replay delivery: got=%+v want=%+v", sync.Cursor, after)
	}
	var replay eventFrame
	if err := wsjson.Read(context.Background(), connection, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Cursor.EventSequence != 1 || replay.Event.ID != "replay-one" {
		t.Fatalf("first replay=%+v", replay)
	}
}

func TestGatewayConvertsUpstreamLossIntoStructuredGap(t *testing.T) {
	controller := newGatewayController()
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS()})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	subscription, err := gateway.hub.Subscribe(events.Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	controller.emit(room.Event{Type: room.EventWarning, Text: "private overflow details", StreamGap: 3})
	select {
	case delivery := <-subscription.Events:
		if delivery.Gap == nil || delivery.Gap.Reason != events.GapUpstreamOverflow {
			t.Fatalf("delivery=%+v", delivery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for structured upstream gap")
	}
}

func TestGatewayIdleExpiryAndScopeChangeCloseWithoutRevokingDevice(t *testing.T) {
	controller := newGatewayController()
	service := gatewayService(t, controller)
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{
		ListenAddress: "127.0.0.1:0", RoomID: controller.room.ID, Service: service, Devices: store, Assets: remoteui.FS(),
		SessionIdleTTL: time.Second, SessionAbsoluteTTL: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	authenticated := authenticateTestDevice(t, client, gateway, store, controller.room.ID, []device.Scope{device.ScopeObserve, device.ScopeParticipate})
	connection := dialEvents(t, gateway, controller.room.ID, authenticated.Cookie, events.Cursor{})
	var sync syncFrame
	if err := wsjson.Read(context.Background(), connection, &sync); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetScopes(authenticated.DeviceID, []device.Scope{device.ScopeObserve}); err != nil {
		t.Fatal(err)
	}
	readContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var frame any
	err = wsjson.Read(readContext, connection, &frame)
	if websocket.CloseStatus(err) != websocket.StatusCode(4001) {
		t.Fatalf("scope change close=%v status=%d", err, websocket.CloseStatus(err))
	}
	connection.CloseNow()
	if _, err := store.NewChallenge(authenticated.DeviceID, time.Second); err != nil {
		t.Fatalf("scope change incorrectly revoked device: %v", err)
	}

	renewed := renewTestDeviceSession(t, client, gateway, authenticated.DeviceID, authenticated.PrivateKey)
	connection = dialEvents(t, gateway, controller.room.ID, renewed.Cookie, events.Cursor{})
	defer connection.CloseNow()
	if err := wsjson.Read(context.Background(), connection, &sync); err != nil {
		t.Fatal(err)
	}
	readContext, cancelIdle := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelIdle()
	err = wsjson.Read(readContext, connection, &frame)
	if websocket.CloseStatus(err) != websocket.StatusCode(4001) {
		t.Fatalf("idle expiry close=%v status=%d", err, websocket.CloseStatus(err))
	}
}

func TestGatewayUsesPublicHTTPSOriginForCookieSecurity(t *testing.T) {
	controller := newGatewayController()
	store, err := device.Open(filepath.Join(testDeviceStateDir(t), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gateway, err := Start(Config{
		ListenAddress: "127.0.0.1:0", Origin: "https://phone.example", RoomID: controller.room.ID,
		Service: gatewayService(t, controller), Devices: store, Assets: remoteui.FS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if !gateway.secure {
		t.Fatal("HTTPS tunnel origin did not enable secure-cookie/HSTS posture")
	}
}

type testDeviceSession struct {
	DeviceID   string
	PrivateKey *ecdsa.PrivateKey
	Cookie     string
	CSRFToken  string
}

func authenticateTestDevice(t *testing.T, client *http.Client, gateway *Gateway, store *device.Store, roomID string, scopes []device.Scope) testDeviceSession {
	t.Helper()
	invitation, err := store.CreateInvitation(roomID, "test device", scopes, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	response := postGateway(t, client, gateway.Origin(), "/api/v1/pair", pairRequest{Code: invitation.Code, Name: "ignored", PublicKey: base64.StdEncoding.EncodeToString(publicKey)}, "", "")
	var paired pairResult
	decodeResponse(t, response, &paired)
	renewed := renewTestDeviceSession(t, client, gateway, paired.DeviceID, privateKey)
	return testDeviceSession{DeviceID: paired.DeviceID, PrivateKey: privateKey, Cookie: renewed.Cookie, CSRFToken: renewed.CSRFToken}
}

func renewTestDeviceSession(t *testing.T, client *http.Client, gateway *Gateway, deviceID string, privateKey *ecdsa.PrivateKey) testDeviceSession {
	t.Helper()
	response := postGateway(t, client, gateway.Origin(), "/api/v1/challenge", challengeRequest{DeviceID: deviceID}, "", "")
	var challenge challengeResult
	decodeResponse(t, response, &challenge)
	response = postGateway(t, client, gateway.Origin(), "/api/v1/session", sessionRequest{
		DeviceID: deviceID, ChallengeID: challenge.ChallengeID,
		Signature: base64.StdEncoding.EncodeToString(signRawP256(t, privateKey, []byte(challenge.Payload))),
	}, "", "")
	cookies := response.Cookies()
	var session sessionResult
	decodeResponse(t, response, &session)
	if len(cookies) != 1 {
		t.Fatalf("session cookies=%d", len(cookies))
	}
	return testDeviceSession{DeviceID: deviceID, PrivateKey: privateKey, Cookie: cookies[0].String(), CSRFToken: session.CSRFToken}
}

func dialEvents(t *testing.T, gateway *Gateway, roomID, cookie string, after events.Cursor) *websocket.Conn {
	t.Helper()
	values := url.Values{"room_id": []string{roomID}, "after_event": []string{strconv.FormatUint(after.EventSequence, 10)}, "after_message": []string{strconv.FormatUint(after.MessageSequence, 10)}}
	if after.BootID != "" {
		values.Set("boot_id", after.BootID)
	}
	streamURL := "ws://" + gateway.Addr() + "/api/v1/events?" + values.Encode()
	headers := http.Header{"Origin": []string{gateway.Origin()}, "Cookie": []string{cookie}}
	connection, _, err := websocket.Dial(context.Background(), streamURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func gatewayService(t *testing.T, controller *gatewayController) *api.Service {
	t.Helper()
	service, err := api.NewService(api.Credentials{
		InstanceID: "gateway-test-instance",
		Entries:    []api.Credential{{ID: "local", Token: "local-secret-token-that-is-long-enough", Kind: api.ClientLocal, Scopes: []api.Scope{api.ScopeObserve}}},
	}, controller)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testDeviceStateDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("remote device state requires POSIX private-file modes; native Windows support is preview")
	}
	return filepath.Join(testutil.CanonicalTempDir(t), "state")
}

func postGateway(t *testing.T, client *http.Client, origin, path string, value any, cookie, csrf string) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, origin+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		request.Header.Set("X-MoHuddle-CSRF", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func signRawP256(t *testing.T, key *ecdsa.PrivateKey, payload []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(payload)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return append(paddedInteger(r, 32), paddedInteger(s, 32)...)
}

func paddedInteger(value *big.Int, size int) []byte {
	result := make([]byte, size)
	value.FillBytes(result)
	return result
}
