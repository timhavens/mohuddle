//go:build !windows

package chatgpt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/roommanager"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

func managerBridge(t *testing.T) (*Bridge, *roommanager.Router, *store.Store, map[string]*api.Service) {
	t.Helper()
	root := testutil.ShortTempDir(t)
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]*api.Service{}
	orchestrators := map[string]*room.Orchestrator{}
	open := func(id string) (*api.Service, error) {
		if service := services[id]; service != nil {
			return service, nil
		}
		state, err := s.LoadRoom(id)
		if err != nil {
			return nil, err
		}
		messages, err := s.LoadMessages(id)
		if err != nil {
			return nil, err
		}
		o, err := room.New(state, messages, s, respondingPeer{chat.Codex})
		if err != nil {
			return nil, err
		}
		go func() {
			for range o.Events() {
			}
		}()
		service, err := api.NewService(*credentials, o)
		if err != nil {
			return nil, err
		}
		server, err := api.StartLocal(filepath.Join(root, id+".sock"), service, nil)
		if err != nil {
			return nil, err
		}
		service.ConfigureChatGPT(server.Addr(), filepath.Join(root, id+".json"), nil)
		services[id] = service
		orchestrators[id] = o
		t.Cleanup(func() { _ = service.RevokeChatGPT(); _ = server.Close(); _ = o.Close() })
		return service, nil
	}
	if _, err := open(initial.ID); err != nil {
		t.Fatal(err)
	}
	router, err := roommanager.New(s, root, open)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := api.NewService(*credentials, orchestrators[initial.ID])
	if err != nil {
		t.Fatal(err)
	}
	gateway.ConfigureManager(router)
	server, err := api.StartLocal(filepath.Join(root, "manager.sock"), gateway, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, err := router.Enable(server.Addr(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { router.Close(); _ = server.Close() })
	return bridge, router, s, services
}

func roomTool(t *testing.T, c *mcp.ClientSession, conversation, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: mcp.Meta{"openai/session": conversation}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func roomToolValue[T any](t *testing.T, c *mcp.ClientSession, conversation, name string, args any) T {
	t.Helper()
	result := roomTool(t, c, conversation, name, args)
	if result.IsError {
		t.Fatalf("%s: %+v", name, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSharedManagerMCPSeparatesConversationsAndRetries(t *testing.T) {
	b, router, s, services := managerBridge(t)
	a, c := mcpClient(t, b), mcpClient(t, b)
	const ca, cb = "conversation-a-private", "conversation-b-private"
	if result := roomTool(t, a, ca, "mohuddle_create_room", CreateRoomInput{}); !result.IsError {
		t.Fatal("empty creation identifier was accepted")
	}
	choices := roomToolValue[api.ChatGPTView](t, a, ca, "mohuddle_join", JoinInput{})
	if !choices.SelectionRequired || choices.ParticipationID != "" || len(choices.Rooms) != 1 || choices.Rooms[0].RoomName != "Room 1" {
		t.Fatalf("unexpected default selection: %+v", choices)
	}
	va := roomToolValue[api.ChatGPTView](t, a, ca, "mohuddle_join", JoinInput{Room: "rOoM1"})
	vb := roomToolValue[api.ChatGPTView](t, c, cb, "mohuddle_create_room", CreateRoomInput{OperationID: "create-one"})
	if vb.RoomName != "Room 2" || va.RoomID == vb.RoomID || va.ParticipationID == vb.ParticipationID {
		t.Fatalf("room isolation failed: %+v %+v", va, vb)
	}
	retry := roomToolValue[api.ChatGPTView](t, c, cb, "mohuddle_create_room", CreateRoomInput{OperationID: "create-one"})
	if retry.RoomID != vb.RoomID || retry.ParticipationID != vb.ParticipationID {
		t.Fatal("creation retry duplicated room or participation")
	}
	if got := roomTool(t, c, cb, "mohuddle_join", JoinInput{Room: "room1"}); !got.IsError {
		t.Fatal("second conversation stole room")
	}
	if _, err := services[va.RoomID].ControlFollowUps("off"); err != nil {
		t.Fatal(err)
	}
	if err := services[va.RoomID].SetChatGPTLimits(chat.ChatGPTLimits{Exchanges: 7, FollowUps: 9, FollowUpSeconds: 3600, RepeatedRequests: 3}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		c         *mcp.ClientSession
		key       string
		view      api.ChatGPTView
		enabled   bool
		exchanges int
	}{
		{a, ca, va, false, 7}, {c, cb, vb, true, 32},
	} {
		read := roomToolValue[api.ChatGPTView](t, item.c, item.key, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: item.view.ParticipationID})
		if read.FollowUps.Enabled != item.enabled || read.State.Limits.Exchanges != item.exchanges {
			t.Fatalf("room policy leaked: %+v", read)
		}
	}
	connection, err := api.ReadChatGPTConnection(router.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Enable(connection.Socket, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	renewed, err := api.ReadChatGPTConnection(router.Path())
	if err != nil || renewed.Token != connection.Token || !renewed.ExpiresAt.After(connection.ExpiresAt) {
		t.Fatal("renewal replaced other rooms' transport authority", err)
	}
	for _, v := range []struct {
		c    *mcp.ClientSession
		key  string
		view api.ChatGPTView
		text string
	}{{a, ca, va, "only room one"}, {c, cb, vb, "only room two"}} {
		roomToolValue[PublishOutput](t, v.c, v.key, "mohuddle_publish", api.ChatGPTPublishRequest{ParticipationID: v.view.ParticipationID, OperationID: "same-id-in-each-room", Text: v.text})
		read := roomToolValue[api.ChatGPTView](t, v.c, v.key, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: v.view.ParticipationID, Limit: 100})
		if read.RoomID != v.view.RoomID || read.RoomName != v.view.RoomName || len(read.Messages) != 1 || read.Messages[0].Text != v.text {
			t.Fatalf("mixed room context: %+v", read)
		}
	}
	// A long poll in one room must not hold the manager routing mutex.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var v api.ChatGPTView
		done <- b.Call(ctx, "chatgpt.read", api.ChatGPTReadRequest{ParticipationID: va.ParticipationID, After: 1, WaitSeconds: 25}, &v)
	}()
	read := roomToolValue[api.ChatGPTView](t, c, cb, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: vb.ParticipationID})
	if read.RoomID != vb.RoomID {
		t.Fatal("long poll changed routing")
	}
	cancel()
	<-done
	if err := router.SetRoomEnabled(va.RoomID, false); err != nil {
		t.Fatal(err)
	}
	if err := services[va.RoomID].RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	if got := roomTool(t, a, ca, "mohuddle_join", JoinInput{Room: "room1"}); !got.IsError {
		t.Fatal("join bypassed host revocation")
	}
	roomToolValue[api.ChatGPTView](t, c, cb, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: vb.ParticipationID})
	foreign, err := s.Create(filepath.Join(s.Root(), "another-project"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := roomTool(t, c, cb, "mohuddle_join", JoinInput{Room: foreign.ID}); !got.IsError {
		t.Fatal("joined unapproved workspace")
	}
	if err := b.Doctor(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSwitchInvalidatesOnlyOldPanelsAndKeepsBinding(t *testing.T) {
	b, _, _, _ := managerBridge(t)
	c := mcpClient(t, b)
	const key = "conversation-switch-private"
	old := roomToolValue[api.ChatGPTView](t, c, key, "mohuddle_join", JoinInput{Room: "Room 1"})
	next := roomToolValue[api.ChatGPTView](t, c, key, "mohuddle_create_room", CreateRoomInput{OperationID: "next-room"})
	if got := roomTool(t, c, key, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: old.ParticipationID}); !got.IsError {
		t.Fatal("old panel still attached")
	}
	reconnected, err := NewFromFile(b.connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	fresh := roomToolValue[api.ChatGPTView](t, mcpClient(t, reconnected), key, "mohuddle_join", JoinInput{})
	if fresh.RoomID != next.RoomID || fresh.ParticipationID != next.ParticipationID {
		t.Fatal("transport restart lost selection")
	}
	other := roomToolValue[api.ChatGPTView](t, c, "another-private-conversation", "mohuddle_join", JoinInput{Room: "room1"})
	if other.RoomID != old.RoomID {
		t.Fatal("detached seat was not released")
	}
	var view api.ChatGPTView
	if err := b.Call(t.Context(), "command.invoke", map[string]string{"command": "stop"}, &view); err == nil || !strings.HasPrefix(err.Error(), "forbidden:") {
		t.Fatal("manager allowed generic controls", err)
	}
}

func TestUnusedRouterCannotRemoveLiveManagerConnection(t *testing.T) {
	b, _, s, _ := managerBridge(t)
	other, err := roommanager.New(s, s.Root(), nil)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err := NewFromFile(b.connectionPath); err != nil {
		t.Fatalf("failed second startup removed live connection: %v", err)
	}
}

func TestCreationRetryCannotResurrectDeletedRoom(t *testing.T) {
	b, _, s, _ := managerBridge(t)
	c := mcpClient(t, b)
	const key = "deletion-private-conversation"
	created := roomToolValue[api.ChatGPTView](t, c, key, "mohuddle_create_room", CreateRoomInput{OperationID: "create-delete"})
	roomToolValue[LeaveOutput](t, c, key, "mohuddle_leave", api.ChatGPTLeaveRequest{ParticipationID: created.ParticipationID})
	if _, err := s.DeleteRoom(created.RoomID); err != nil {
		t.Fatal(err)
	}
	if got := roomTool(t, c, key, "mohuddle_create_room", CreateRoomInput{OperationID: "create-delete"}); !got.IsError {
		t.Fatal("deleted room resurrected")
	}
}

func TestExplicitRoomTransferLeavesOtherConversationUntouched(t *testing.T) {
	b, _, _, _ := managerBridge(t)
	a, c, d := mcpClient(t, b), mcpClient(t, b), mcpClient(t, b)
	va := roomToolValue[api.ChatGPTView](t, a, "chat-a", "mohuddle_join", JoinInput{Room: "room1"})
	vb := roomToolValue[api.ChatGPTView](t, c, "chat-b", "mohuddle_create_room", CreateRoomInput{OperationID: "create-b"})
	if !roomTool(t, d, "chat-new", "mohuddle_join", JoinInput{ReplaceExisting: true}).IsError {
		t.Fatal("transfer without named room accepted")
	}
	vc := roomToolValue[api.ChatGPTView](t, d, "chat-new", "mohuddle_join", JoinInput{Room: va.RoomName, ReplaceExisting: true})
	if vc.RoomID != va.RoomID || vc.ParticipationID == va.ParticipationID {
		t.Fatal("transfer failed")
	}
	if !roomTool(t, a, "chat-a", "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: va.ParticipationID}).IsError {
		t.Fatal("old room attachment accepted")
	}
	read := roomToolValue[api.ChatGPTView](t, c, "chat-b", "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: vb.ParticipationID, PanelToken: vb.PanelToken})
	if read.RoomID != vb.RoomID || read.PanelToken != vb.PanelToken {
		t.Fatal("transfer changed the other room")
	}
}

func TestSinglePanelAcrossJoinsAndManagerRestart(t *testing.T) {
	b, router, s, services := managerBridge(t)
	c := mcpClient(t, b)
	const conversation = "stable-panel-conversation"
	joined := roomToolValue[api.ChatGPTView](t, c, conversation, "mohuddle_join", JoinInput{Room: "room1"})
	if joined.PanelState != "not_opened" || joined.PanelToken != "" {
		t.Fatal("join rendered a panel")
	}
	opened := roomToolValue[api.ChatGPTView](t, c, conversation, "mohuddle_panel", api.ChatGPTPanelRequest{ParticipationID: joined.ParticipationID})
	for range 3 {
		rejoined := roomToolValue[api.ChatGPTView](t, c, conversation, "mohuddle_join", JoinInput{})
		if rejoined.PanelState != "opened" || rejoined.PanelToken != opened.PanelToken || rejoined.ParticipationID != opened.ParticipationID {
			t.Fatal("join replaced a healthy panel")
		}
		if !roomTool(t, c, conversation, "mohuddle_panel", api.ChatGPTPanelRequest{ParticipationID: joined.ParticipationID}).IsError {
			t.Fatal("duplicate render succeeded")
		}
	}
	replacement := roomToolValue[api.ChatGPTView](t, c, conversation, "mohuddle_panel", api.ChatGPTPanelRequest{ParticipationID: joined.ParticipationID, ReplaceExisting: true})
	if replacement.PanelToken == opened.PanelToken {
		t.Fatal("explicit replacement retained old token")
	}
	if !roomTool(t, c, conversation, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: opened.ParticipationID, PanelToken: opened.PanelToken}).IsError {
		t.Fatal("old panel stayed active")
	}
	if err := services[joined.RoomID].RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	router.Close()
	restarted, err := roommanager.New(s, s.Root(), func(id string) (*api.Service, error) { return services[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	path, err := restarted.Enable(filepath.Join(s.Root(), "manager.sock"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := api.ReadChatGPTConnection(path)
	if err != nil {
		t.Fatal(err)
	}
	session := restarted.Authenticate(api.HelloRequest{Token: connection.Token})
	hash := sha256.Sum256([]byte(conversation))
	payload, _ := json.Marshal(api.ManagedJoinRequest{ClientKey: fmt.Sprintf("%x", hash)})
	r := restarted.Handle(t.Context(), session, api.Request{Version: api.Version, ID: "join", Type: "chatgpt.join", Payload: payload}).Response
	if !r.OK {
		t.Fatal(r.Error)
	}
	view := r.Result.(api.ChatGPTView)
	if view.RoomID != joined.RoomID || view.PanelState != "reopen_required" {
		t.Fatal("restart lost panel history", view.PanelState)
	}
	payload, _ = json.Marshal(api.ChatGPTPanelRequest{ParticipationID: view.ParticipationID})
	r = restarted.Handle(t.Context(), session, api.Request{Version: api.Version, ID: "panel", Type: "chatgpt.panel", Payload: payload}).Response
	if r.OK || r.Error.Code != "panel_exists" {
		t.Fatal("restart permitted automatic duplicate", r.Error)
	}
	payload, _ = json.Marshal(api.ChatGPTPanelRequest{ParticipationID: view.ParticipationID, ReplaceExisting: true})
	r = restarted.Handle(t.Context(), session, api.Request{Version: api.Version, ID: "replace", Type: "chatgpt.panel", Payload: payload}).Response
	if !r.OK {
		t.Fatal("requested replacement failed after restart", r.Error)
	}
}

func TestLegacyConversationRequiresExplicitPanelRequest(t *testing.T) {
	b, router, s, services := managerBridge(t)
	c := mcpClient(t, b)
	const key = "pre-upgrade-conversation"
	joined := roomToolValue[api.ChatGPTView](t, c, key, "mohuddle_join", JoinInput{Room: "room1"})
	if err := services[joined.RoomID].RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	router.Close()
	path := filepath.Join(s.Root(), "room_routing.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "panel_requested")
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := roommanager.New(s, s.Root(), func(id string) (*api.Service, error) { return services[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	connectionPath, err := restarted.Enable(filepath.Join(s.Root(), "manager.sock"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := api.ReadChatGPTConnection(connectionPath)
	if err != nil {
		t.Fatal(err)
	}
	session := restarted.Authenticate(api.HelloRequest{Token: connection.Token})
	hash := sha256.Sum256([]byte(key))
	payload, _ := json.Marshal(api.ManagedJoinRequest{ClientKey: fmt.Sprintf("%x", hash)})
	r := restarted.Handle(t.Context(), session, api.Request{Version: api.Version, ID: "join", Type: "chatgpt.join", Payload: payload}).Response
	if !r.OK || r.Result.(api.ChatGPTView).PanelState != "reopen_required" {
		t.Fatal("legacy conversation guessed no panel", r.Error)
	}
}
