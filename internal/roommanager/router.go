// Package roommanager routes one private ChatGPT connection to isolated rooms.
package roommanager

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

type routingState struct {
	PanelRequested map[string]bool   `json:"panel_requested,omitempty"`
	Created        map[string]bool   `json:"created,omitempty"`
	Bindings       map[string]string `json:"bindings"`
	Creations      map[string]string `json:"creations"`
	Disabled       map[string]bool   `json:"disabled"`
}
type attachment struct {
	room, client string
	service      *api.Service
	session      *api.Session
}

type Router struct {
	dispatch    sync.RWMutex
	owned       func(string) bool
	mu          sync.Mutex
	store       *store.Store
	workspace   string
	open        func(string) (*api.Service, error)
	state       routingState
	attachments map[string]attachment
	connection  api.ChatGPTConnection
	path        string
}

func New(s *store.Store, workspace string, open func(string) (*api.Service, error)) (*Router, error) {
	canonical, err := access.CanonicalDirectory(workspace)
	if err != nil {
		return nil, err
	}
	r := &Router{store: s, workspace: canonical, open: open, attachments: map[string]attachment{}, path: filepath.Join(s.Root(), "chatgpt-manager.json")}
	if data, err := os.ReadFile(filepath.Join(s.Root(), "room_routing.json")); err == nil {
		if err := json.Unmarshal(data, &r.state); err != nil {
			return nil, fmt.Errorf("read room routing: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if r.state.Bindings == nil {
		r.state.Bindings = map[string]string{}
	}
	if r.state.Creations == nil {
		r.state.Creations = map[string]string{}
	}
	if r.state.Disabled == nil {
		r.state.Disabled = map[string]bool{}
	}
	if r.state.Created == nil {
		r.state.Created = map[string]bool{}
	}
	if r.state.PanelRequested == nil {
		r.state.PanelRequested = map[string]bool{}
	}
	return r, nil
}

func (r *Router) ConfigureOwnership(probe func(string) bool) { r.owned = probe }
func (r *Router) WithDispatchPaused(fn func() error) error {
	r.dispatch.Lock()
	defer r.dispatch.Unlock()
	return fn()
}

func (r *Router) save() error {
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	return r.store.WritePrivateState("room_routing.json", data)
}

func (r *Router) Enable(socket string, ttl time.Duration) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ttl < time.Minute || ttl > 24*time.Hour {
		return "", fmt.Errorf("access duration must be between 1m and 24h")
	}
	if time.Now().Before(r.connection.ExpiresAt) {
		// Renew the shared transport lifetime without invalidating other rooms'
		// participation capabilities. Their own pauses and budgets stay separate.
		value := r.connection
		if expiry := time.Now().Add(ttl); expiry.After(value.ExpiresAt) {
			value.ExpiresAt = expiry
			if err := api.WriteChatGPTConnection(r.path, value); err != nil {
				return "", err
			}
			r.connection = value
		}
		return r.path, nil
	}
	token, err := api.NewChatGPTToken()
	if err != nil {
		return "", err
	}
	value := api.ChatGPTConnection{Version: api.ChatGPTManagerConnectionVersion, Socket: socket, RoomID: "manager", Token: token, ExpiresAt: time.Now().Add(ttl)}
	if err := api.WriteChatGPTConnection(r.path, value); err != nil {
		return "", err
	}
	r.connection = value
	r.attachments = map[string]attachment{}
	return r.path, nil
}

func (r *Router) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Before(r.connection.ExpiresAt)
}
func (r *Router) Path() string { return r.path }
func (r *Router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A failed second manager startup must never remove the active manager's
	// connection file. Remove only the grant this Router actually published.
	if r.connection.Token != "" {
		data, err := os.ReadFile(r.path)
		var current api.ChatGPTConnection
		if err == nil && json.Unmarshal(data, &current) == nil && current.Token == r.connection.Token {
			_ = os.Remove(r.path)
		}
	}
	r.connection = api.ChatGPTConnection{}
	r.attachments = map[string]attachment{}
}

func (r *Router) SetRoomEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.state.Disabled[id]
	r.state.Disabled[id] = !enabled
	if err := r.save(); err != nil {
		r.state.Disabled[id] = old
		return err
	}
	if !enabled {
		for p, a := range r.attachments {
			if a.room == id {
				a.service.DetachChatGPT(p)
				delete(r.attachments, p)
			}
		}
	}
	return nil
}

func (r *Router) Authenticate(value api.HelloRequest) *api.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !time.Now().Before(r.connection.ExpiresAt) || subtle.ConstantTimeCompare([]byte(value.Token), []byte(r.connection.Token)) != 1 {
		return nil
	}
	return &api.Session{Manager: true, Identity: "chatgpt-manager", InstanceID: "manager", Credential: value.Token, Kind: api.ClientChatGPT, RoomID: "manager", Scopes: map[api.Scope]bool{api.ScopeObserve: true, api.ScopeParticipate: true}}
}

func (r *Router) directoryLocked() ([]api.ManagedRoomView, error) {
	names, err := r.store.RoomNames()
	if err != nil {
		return nil, err
	}
	rooms, err := r.store.ListRooms()
	if err != nil {
		return nil, err
	}
	result := []api.ManagedRoomView{}
	for _, room := range rooms {
		if !r.workspaceAllowed(room.Workspace) {
			continue
		}
		v := api.ManagedRoomView{RoomID: room.ID, RoomName: names[room.ID], Status: "saved", Available: !r.state.Disabled[room.ID], UpdatedAt: room.UpdatedAt}
		if room.Coordination != nil && room.Coordination.Objective != nil {
			v.Objective = room.Coordination.Objective.Summary
		}
		if !v.Available {
			v.Status = "access disabled by host"
		}
		owned := false
		for _, a := range r.attachments {
			if a.room == room.ID {
				owned = true
				state, _ := a.service.ChatGPTStatus()
				if state.Connected {
					v.Status = "ChatGPT connected"
					v.Available = false
				}
				if c, err := a.service.CoordinationStatus(time.Now()); err == nil && c != nil {
					v.Status = c.Summary
				}
				break
			}
		}
		if !owned {
			if inUse, _, err := r.store.PeekRoomInUse(room.ID); err != nil {
				v.Status = "availability unknown"
				v.Available = false
			} else if inUse {
				v.Status = "open"
				if r.owned != nil && !r.owned(room.ID) {
					v.Status = "open in another MoHuddle process"
					v.Available = false
				}
			}
		}
		result = append(result, v)
	}
	return result, nil
}

func (r *Router) workspaceAllowed(workspace string) bool {
	canonical, err := access.CanonicalDirectory(workspace)
	return err == nil && canonical == r.workspace
}

func (r *Router) Handle(ctx context.Context, session *api.Session, req api.Request) api.HandleResult {
	if req.Type != "chatgpt.read" && req.Type != "chatgpt.read_message" && req.Type != "chatgpt.rooms" {
		r.dispatch.RLock()
		defer r.dispatch.RUnlock()
	}
	r.mu.Lock()
	if !time.Now().Before(r.connection.ExpiresAt) || subtle.ConstantTimeCompare([]byte(session.Credential), []byte(r.connection.Token)) != 1 {
		r.mu.Unlock()
		return api.ManagerFailure(req, "authentication_failed", "manager access expired; enable ChatGPT in MoHuddle")
	}
	if req.Type == "chatgpt.rooms" {
		rooms, err := r.directoryLocked()
		r.mu.Unlock()
		if err != nil {
			return api.ManagerFailure(req, "directory_unavailable", err.Error())
		}
		return api.ManagerSuccess(req, api.ChatGPTView{SelectionRequired: true, Rooms: rooms, Usage: "Offer these rooms by name, or create a new room only when the human asks. Do not pick a room implicitly."})
	}
	if req.Type == "chatgpt.join" || req.Type == "chatgpt.create_room" {
		result := r.joinLocked(ctx, req)
		r.mu.Unlock()
		return result
	}
	if !strings.HasPrefix(req.Type, "chatgpt.") {
		r.mu.Unlock()
		return api.ManagerFailure(req, "forbidden", "manager grants allow only ChatGPT room tools")
	}
	var identity struct {
		ParticipationID string `json:"participation_id"`
		ReplaceExisting bool   `json:"replace_existing,omitempty"`
	}
	if json.Unmarshal(req.Payload, &identity) != nil || identity.ParticipationID == "" {
		r.mu.Unlock()
		return api.ManagerFailure(req, "not_joined", "select a room and join before using room tools")
	}
	a, ok := r.attachments[identity.ParticipationID]
	if !ok || r.state.Disabled[a.room] {
		r.mu.Unlock()
		return api.ManagerFailure(req, "not_joined", "this room attachment ended; join the intended room again")
	}
	if req.Type == "chatgpt.panel" {
		key := a.client + ":" + a.room
		previous := r.state.PanelRequested[key]
		if previous && !identity.ReplaceExisting {
			r.mu.Unlock()
			return api.ManagerFailure(req, "panel_exists", "reuse the existing panel; only open a replacement when the user requests it, with replace_existing=true")
		}
		// Record the request before rendering. Unknown delivery must never cause
		// automatic replacement cards after a retry or manager restart.
		r.state.PanelRequested[key] = true
		if err := r.save(); err != nil {
			r.state.PanelRequested[key] = previous
			r.mu.Unlock()
			return api.ManagerFailure(req, "persistence_failed", "could not save panel request; no panel was opened")
		}
	}
	// Only the dedicated room API's allowlist can execute routed requests.
	roomRequest := req
	roomRequest.RoomID = a.room
	r.mu.Unlock()
	result := a.service.Handle(ctx, a.session, roomRequest)
	if v, ok := result.Response.Result.(api.ChatGPTView); ok {
		names, _ := r.store.RoomNames()
		v.RoomName = names[a.room]
		result.Response.Result = v
	}
	if req.Type == "chatgpt.leave" && result.Response.OK {
		r.mu.Lock()
		delete(r.attachments, identity.ParticipationID)
		r.mu.Unlock()
	}
	return result
}

func (r *Router) joinLocked(ctx context.Context, req api.Request) api.HandleResult {
	var input api.ManagedJoinRequest
	if json.Unmarshal(req.Payload, &input) != nil || len(input.ClientKey) < 16 || len(input.ClientKey) > 128 {
		return api.ManagerFailure(req, "invalid_request", "a stable conversation identity is required")
	}
	selector := input.Room
	if input.ReplaceExisting && (selector == "" || req.Type != "chatgpt.join") {
		return api.ManagerFailure(req, "invalid_request", "transferring room control requires an explicitly selected room on join")
	}
	if req.Type == "chatgpt.create_room" {
		if input.OperationID == "" || len(input.OperationID) > 128 || strings.ContainsAny(input.OperationID, " \n\r\t") {
			return api.ManagerFailure(req, "invalid_request", "a unique operation_id is required")
		}
		key := input.ClientKey + ":" + input.OperationID
		id := r.state.Creations[key]
		if id == "" {
			var err error
			id, err = store.NewID()
			if err != nil {
				return api.ManagerFailure(req, "create_failed", err.Error())
			}
			r.state.Creations[key] = id
			if err := r.save(); err != nil {
				delete(r.state.Creations, key)
				return api.ManagerFailure(req, "create_failed", err.Error())
			}
		}
		if _, err := r.store.LoadRoom(id); os.IsNotExist(err) {
			if r.state.Created[key] {
				return api.ManagerFailure(req, "room_deleted", "the room created by this operation was deleted; use a new operation only for a newly requested room")
			}
			if err := r.store.SaveRoom(chat.NewRoom(id, r.workspace, 1, time.Now().UTC())); err != nil {
				return api.ManagerFailure(req, "create_failed", err.Error())
			}
		} else if err != nil {
			return api.ManagerFailure(req, "create_failed", err.Error())
		}
		r.state.Created[key] = true
		if err := r.save(); err != nil {
			return api.ManagerFailure(req, "persistence_failed", "room exists but its creation receipt could not be saved; retry the same operation")
		}
		selector = id
	}
	if selector == "" {
		selector = r.state.Bindings[input.ClientKey]
	}
	if selector == "" {
		rooms, err := r.directoryLocked()
		if err != nil {
			return api.ManagerFailure(req, "directory_unavailable", err.Error())
		}
		return api.ManagerSuccess(req, api.ChatGPTView{SelectionRequired: true, Rooms: rooms, Usage: "Ask which room to join, or whether to create a new room. Use its simple name in mohuddle_join."})
	}
	room, err := r.store.ResolveRoom(selector)
	if err != nil {
		return api.ManagerFailure(req, "room_not_found", "room not found; list rooms and select one")
	}
	if !r.workspaceAllowed(room.Workspace) {
		return api.ManagerFailure(req, "access_denied", "this connection is limited to the current project")
	}
	if r.state.Disabled[room.ID] {
		return api.ManagerFailure(req, "access_denied", "the host disabled access to this room; enable it in MoHuddle")
	}
	service, err := r.open(room.ID)
	if err != nil {
		return api.ManagerFailure(req, "room_unavailable", err.Error())
	}
	remaining := time.Until(r.connection.ExpiresAt)
	if remaining < time.Minute {
		return api.ManagerFailure(req, "authentication_failed", "manager access is expiring; renew it in MoHuddle")
	}
	path, err := service.EnsureChatGPT(remaining)
	if err != nil {
		return api.ManagerFailure(req, "room_unavailable", err.Error())
	}
	connection, err := api.ReadChatGPTConnection(path)
	if err != nil {
		return api.ManagerFailure(req, "room_unavailable", "room access is unavailable")
	}
	localSession, err := service.Authenticate(api.HelloRequest{ClientID: "manager", Token: connection.Token})
	if err != nil {
		return api.ManagerFailure(req, "room_unavailable", "room access is unavailable")
	}
	panelKey := input.ClientKey + ":" + room.ID
	panelRequested, tracked := r.state.PanelRequested[panelKey]
	if !tracked && r.state.Bindings[input.ClientKey] == room.ID {
		// A pre-upgrade conversation may already contain a rendered panel.
		panelRequested = true
	}
	payload, _ := json.Marshal(api.ChatGPTJoinRequest{ClientKey: input.ClientKey, ReplaceExisting: input.ReplaceExisting, PanelPreviouslyOpened: panelRequested})
	localReq := req
	localReq.Type = "chatgpt.join"
	localReq.RoomID = room.ID
	localReq.Payload = payload
	result := service.Handle(ctx, localSession, localReq)
	if !result.Response.OK {
		return result
	}
	view, ok := result.Response.Result.(api.ChatGPTView)
	if !ok {
		return api.ManagerFailure(req, "internal_error", "invalid room join result")
	}
	old := r.state.Bindings[input.ClientKey]
	r.state.Bindings[input.ClientKey] = room.ID
	r.state.PanelRequested[panelKey] = panelRequested
	if err := r.save(); err != nil {
		r.state.Bindings[input.ClientKey] = old
		if !tracked {
			delete(r.state.PanelRequested, panelKey)
		}
		service.DetachChatGPT(view.ParticipationID)
		return api.ManagerFailure(req, "persistence_failed", "could not save room selection")
	}
	for p, a := range r.attachments {
		if a.client == input.ClientKey || a.room == room.ID {
			if p != view.ParticipationID {
				a.service.DetachChatGPT(p)
				delete(r.attachments, p)
			}
		}
	}
	r.attachments[view.ParticipationID] = attachment{room.ID, input.ClientKey, service, localSession}
	names, err := r.store.RoomNames()
	if err != nil {
		return api.ManagerFailure(req, "directory_unavailable", err.Error())
	}
	view.RoomName = names[room.ID]
	result.Response.Result = view
	return result
}
