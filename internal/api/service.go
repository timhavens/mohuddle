package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

type Controller interface {
	Snapshot() (chat.Room, []chat.Message)
	CoreStatus() room.CoreStatus
	PostExternal(string, chat.RouteMetadata) error
	AskExternal(string, chat.RouteMetadata) error
	RoundExternal(string, chat.RouteMetadata) error
	Continue() error
	Stop()
	SetPresence(chat.Participant, bool) error
	SubscribeEvents(int) (<-chan room.Event, func())
}

type Session struct {
	Identity   string
	InstanceID string
	Credential string
	Kind       ClientKind
	Scopes     map[Scope]bool
	RoomID     string
}

func (s Session) Has(scope Scope) bool { return s.Scopes[scope] }

type HandleResult struct {
	Response  Response
	Subscribe bool
}

type Service struct {
	credentials Credentials
	controller  Controller

	dedupMu   sync.Mutex
	seen      map[string]struct{}
	seenOrder []string
	maxSeen   int
}

func NewService(credentials Credentials, controller Controller) (*Service, error) {
	if err := credentials.Validate(); err != nil {
		return nil, err
	}
	if controller == nil {
		return nil, fmt.Errorf("API room controller is required")
	}
	service := &Service{
		credentials: credentials,
		controller:  controller,
		seen:        make(map[string]struct{}),
		maxSeen:     4096,
	}
	_, messages := controller.Snapshot()
	for _, message := range messages {
		if message.Route != nil && validIdentifier(message.Route.MessageID) {
			if _, duplicate := service.seen[message.Route.MessageID]; duplicate {
				continue
			}
			service.seen[message.Route.MessageID] = struct{}{}
			service.seenOrder = append(service.seenOrder, message.Route.MessageID)
		}
	}
	if len(service.seenOrder) > service.maxSeen {
		service.seenOrder = service.seenOrder[len(service.seenOrder)-service.maxSeen:]
		kept := make(map[string]struct{}, len(service.seenOrder))
		for _, id := range service.seenOrder {
			kept[id] = struct{}{}
		}
		service.seen = kept
	}
	return service, nil
}

func (s *Service) InstanceID() string { return s.credentials.InstanceID }

func (s *Service) Authenticate(value HelloRequest) (*Session, error) {
	credential, ok := s.credentials.Authenticate(value.Token)
	if !ok {
		return nil, fmt.Errorf("authentication failed")
	}
	identity, err := namespacedIdentity(s.credentials.InstanceID, credential.ID, value.ClientID)
	if err != nil {
		return nil, err
	}
	scopes := make(map[Scope]bool, len(credential.Scopes))
	for _, scope := range credential.Scopes {
		scopes[scope] = true
	}
	originInstance := s.credentials.InstanceID
	if credential.Kind == ClientPeer {
		originInstance = credential.InstanceID
	}
	return &Session{Identity: identity, InstanceID: originInstance, Credential: credential.ID, Kind: credential.Kind, Scopes: scopes}, nil
}

func (s *Service) Handle(_ context.Context, session *Session, request Request) HandleResult {
	if request.Version != Version {
		return failed(request, "unsupported_version", "supported protocol version is "+Version)
	}
	if !validIdentifier(request.ID) {
		return failed(request, "invalid_request", "request id is required and must be a valid identifier")
	}
	if session == nil {
		return failed(request, "unauthenticated", "hello must be completed first")
	}
	switch request.Type {
	case "room.join":
		return s.joinRoom(session, request)
	case "room.get":
		if result := s.requireJoined(session, request, ScopeObserve); result != nil {
			return *result
		}
		roomState, _ := s.controller.Snapshot()
		return succeeded(request, roomView(roomState))
	case "history.get":
		return s.history(session, request)
	case "status.get":
		return s.status(session, request)
	case "message.send":
		return s.sendMessage(session, request)
	case "command.invoke":
		return s.invokeCommand(session, request)
	case "events.subscribe":
		if result := s.requireJoined(session, request, ScopeObserve); result != nil {
			return *result
		}
		return HandleResult{Response: successResponse(request, map[string]any{"subscribed": true}), Subscribe: true}
	default:
		return failed(request, "unknown_request", "unknown request type")
	}
}

func (s *Service) joinRoom(session *Session, request Request) HandleResult {
	if !session.Has(ScopeObserve) {
		return failed(request, "forbidden", "observe scope is required")
	}
	value, err := decodePayload[JoinRoomRequest](request)
	if err != nil {
		return failed(request, "invalid_request", err.Error())
	}
	roomState, _ := s.controller.Snapshot()
	if value.RoomID == "" || value.RoomID != roomState.ID {
		return failed(request, "room_not_found", "this endpoint does not host the requested room")
	}
	if session.RoomID != "" && session.RoomID != value.RoomID {
		return failed(request, "already_joined", "client identity is already bound to another room")
	}
	session.RoomID = value.RoomID
	return succeeded(request, JoinRoomResult{RoomID: value.RoomID})
}

func (s *Service) history(session *Session, request Request) HandleResult {
	if result := s.requireJoined(session, request, ScopeObserve); result != nil {
		return *result
	}
	value, err := decodePayload[HistoryRequest](request)
	if err != nil {
		return failed(request, "invalid_request", err.Error())
	}
	if value.Limit == 0 {
		value.Limit = DefaultHistory
	}
	if value.Limit < 1 || value.Limit > MaxHistory {
		return failed(request, "invalid_request", fmt.Sprintf("history limit must be between 1 and %d", MaxHistory))
	}
	_, messages := s.controller.Snapshot()
	result := make([]MessageView, 0, value.Limit)
	hasMore := false
	for _, message := range messages {
		if message.Sequence <= value.After {
			continue
		}
		if len(result) == value.Limit {
			hasMore = true
			break
		}
		result = append(result, messageView(message))
	}
	return succeeded(request, HistoryResult{Messages: result, HasMore: hasMore})
}

func (s *Service) status(session *Session, request Request) HandleResult {
	if result := s.requireJoined(session, request, ScopeObserve); result != nil {
		return *result
	}
	roomState, messages := s.controller.Snapshot()
	core := s.controller.CoreStatus()
	total, byAgent := chat.CorrectionStatistics(messages)
	return succeeded(request, StatusResult{
		Room: roomView(roomState), ActiveCores: append([]chat.Participant(nil), core.Active...),
		Availability: cloneAvailability(core.Availability), Corrections: total, ByAgent: byAgent,
	})
}

func (s *Service) sendMessage(session *Session, request Request) HandleResult {
	if result := s.requireJoined(session, request, ScopeParticipate); result != nil {
		return *result
	}
	value, err := decodePayload[SendMessageRequest](request)
	if err != nil {
		return failed(request, "invalid_request", err.Error())
	}
	value.Text = strings.TrimSpace(value.Text)
	if value.Text == "" {
		return failed(request, "invalid_request", "message text is required")
	}
	mode := strings.ToLower(strings.TrimSpace(value.Mode))
	if mode == "" {
		mode = "post"
	}
	if mode != "post" && mode != "ask" && mode != "round" {
		return failed(request, "invalid_request", "message mode must be post, ask, or round")
	}
	if session.Kind != ClientLocal && mode != "ask" {
		return failed(request, "forbidden", "peer and bridge clients are restricted to read-only ask turns")
	}
	if result := s.claimRoute(session, request); result != nil {
		return *result
	}
	route := chat.RouteMetadata{
		MessageID: request.Route.MessageID, OriginInstanceID: request.Route.OriginInstanceID,
		OriginClientID: request.Route.OriginClientID,
		Hops:           append(append([]string(nil), request.Route.Hops...), s.credentials.InstanceID),
	}
	switch mode {
	case "post":
		err = s.controller.PostExternal(value.Text, route)
	case "ask":
		err = s.controller.AskExternal(value.Text, route)
	case "round":
		err = s.controller.RoundExternal(value.Text, route)
	}
	if err != nil {
		return failed(request, "command_failed", err.Error())
	}
	return succeeded(request, map[string]any{"accepted": true, "message_id": request.Route.MessageID})
}

func (s *Service) invokeCommand(session *Session, request Request) HandleResult {
	value, err := decodePayload[InvokeCommandRequest](request)
	if err != nil {
		return failed(request, "invalid_request", err.Error())
	}
	value.Command = strings.ToLower(strings.TrimSpace(value.Command))
	required := ScopeParticipate
	if value.Command == "join" || value.Command == "leave" {
		required = ScopeAdminister
	}
	if result := s.requireJoined(session, request, required); result != nil {
		return *result
	}
	if session.Kind != ClientLocal {
		return failed(request, "forbidden", "remote guests cannot invoke room-control commands")
	}
	switch value.Command {
	case "continue", "stop":
	case "join", "leave":
		if !value.Participant.ValidAgent() {
			return failed(request, "invalid_request", "a valid participant is required")
		}
	default:
		return failed(request, "forbidden", "command is not exposed by the v1 API")
	}
	if result := s.claimRoute(session, request); result != nil {
		return *result
	}
	switch value.Command {
	case "continue":
		err = s.controller.Continue()
	case "stop":
		s.controller.Stop()
	case "join", "leave":
		err = s.controller.SetPresence(value.Participant, value.Command == "join")
	}
	if err != nil {
		return failed(request, "command_failed", err.Error())
	}
	return succeeded(request, map[string]any{"accepted": true, "message_id": request.Route.MessageID})
}

func (s *Service) requireJoined(session *Session, request Request, scope Scope) *HandleResult {
	if !session.Has(scope) {
		result := failed(request, "forbidden", string(scope)+" scope is required")
		return &result
	}
	if session.RoomID == "" {
		result := failed(request, "not_joined", "join a room before making this request")
		return &result
	}
	roomState, _ := s.controller.Snapshot()
	if session.RoomID != roomState.ID || (request.RoomID != "" && request.RoomID != session.RoomID) {
		result := failed(request, "room_not_found", "request does not target the joined room")
		return &result
	}
	return nil
}

func (s *Service) claimRoute(session *Session, request Request) *HandleResult {
	if request.Route == nil || !validIdentifier(request.Route.MessageID) || request.Route.OriginInstanceID != session.InstanceID || request.Route.OriginClientID != session.Identity {
		result := failed(request, "invalid_route", "mutating requests require a unique message id and the authenticated origin identity")
		return &result
	}
	if len(request.Route.Hops) >= MaxRouteHops {
		result := failed(request, "hop_limit", "route hop limit exceeded")
		return &result
	}
	for _, hop := range request.Route.Hops {
		if hop == s.credentials.InstanceID {
			result := failed(request, "routing_loop", "route already contains this MoHuddle instance")
			return &result
		}
		if !validIdentifier(hop) {
			result := failed(request, "invalid_route", "route contains an invalid hop identity")
			return &result
		}
	}
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	if _, duplicate := s.seen[request.Route.MessageID]; duplicate {
		result := failed(request, "duplicate_message", "message id was already processed")
		return &result
	}
	s.seen[request.Route.MessageID] = struct{}{}
	s.seenOrder = append(s.seenOrder, request.Route.MessageID)
	if len(s.seenOrder) > s.maxSeen {
		delete(s.seen, s.seenOrder[0])
		s.seenOrder = s.seenOrder[1:]
	}
	return nil
}

func succeeded(request Request, result any) HandleResult {
	return HandleResult{Response: successResponse(request, result)}
}

func successResponse(request Request, result any) Response {
	return Response{Version: Version, ID: request.ID, OK: true, Result: result}
}

func failed(request Request, code, message string) HandleResult {
	return HandleResult{Response: Response{Version: Version, ID: request.ID, OK: false, Error: &ProtocolError{Code: code, Message: message}}}
}

func roomView(value chat.Room) RoomView {
	members := make(map[chat.Participant]bool, len(value.Members))
	for participant, present := range value.Members {
		members[participant] = present
	}
	var policy *chat.CorePolicy
	if value.CorePolicy != nil {
		copy := *value.CorePolicy
		copy.Preferred = append([]chat.Participant(nil), value.CorePolicy.Preferred...)
		copy.Fallbacks = append([]chat.Participant(nil), value.CorePolicy.Fallbacks...)
		policy = &copy
	}
	var conflict *chat.ConflictState
	if value.Conflict != nil {
		copy := *value.Conflict
		copy.Reasons = make(map[chat.Participant]string, len(value.Conflict.Reasons))
		for participant, reason := range value.Conflict.Reasons {
			copy.Reasons[participant] = reason
		}
		conflict = &copy
	}
	return RoomView{
		ID: value.ID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		Moderator: value.Moderator, Members: members, CorePolicy: policy,
		CorePromotions: append([]chat.CorePromotion(nil), value.CorePromotions...), Conflict: conflict,
	}
}

func messageView(value chat.Message) MessageView {
	attachments := make([]AttachmentView, 0, len(value.Attachments))
	for _, attachment := range value.Attachments {
		attachments = append(attachments, AttachmentView{
			ID: attachment.ID, Kind: attachment.Kind, Name: attachment.Name,
			MIMEType: attachment.MIMEType, Size: attachment.Size,
			Width: attachment.Width, Height: attachment.Height,
		})
	}
	var route *chat.RouteMetadata
	if value.Route != nil {
		copy := *value.Route
		copy.Hops = append([]string(nil), value.Route.Hops...)
		route = &copy
	}
	return MessageView{
		ID: value.ID, Sequence: value.Sequence, Author: value.Author, Target: value.Target,
		Kind: value.Kind, Text: value.Text, Attachments: attachments,
		CorrectionEvents: append([]chat.CorrectionEvent(nil), value.CorrectionEvents...), Route: route, CreatedAt: value.CreatedAt,
	}
}

func cloneAvailability(source map[chat.Participant]chat.ParticipantAvailability) map[chat.Participant]chat.ParticipantAvailability {
	result := make(map[chat.Participant]chat.ParticipantAvailability, len(source))
	for participant, value := range source {
		if value.RetryAt != nil {
			copy := *value.RetryAt
			value.RetryAt = &copy
		}
		result[participant] = value
	}
	return result
}

func NewEvent(instanceID, roomID string, value room.Event, local bool) (Event, error) {
	id, err := NewID()
	if err != nil {
		return Event{}, err
	}
	payload := EventPayload{
		Type: string(value.Type), Participant: value.Participant,
		Participants: append([]chat.Participant(nil), value.Participants...),
		Wave:         value.Wave,
	}
	if local {
		payload.Text = value.Text
	}
	if local && value.Err != nil {
		payload.Error = value.Err.Error()
	}
	if value.Message != nil {
		message := messageView(*value.Message)
		payload.Message = &message
	}
	if value.AgentEvent != nil {
		agentEvent := AgentEventView{Type: string(value.AgentEvent.Type), Agent: value.AgentEvent.Agent}
		if local || string(value.AgentEvent.Type) == "delta" {
			agentEvent.Text = value.AgentEvent.Text
		}
		payload.Agent = &agentEvent
	}
	return Event{
		Version: Version, ID: id, Type: "event", RoomID: roomID,
		Route: Route{MessageID: id, OriginInstanceID: instanceID, OriginClientID: instanceID + "/host", Hops: []string{instanceID}},
		At:    time.Now().UTC(), Payload: payload,
	}, nil
}
