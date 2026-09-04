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
	SetWorkflowMode(chat.WorkflowMode) error
	SetDelegationPolicy(chat.DelegationPolicy) error
	ExecutePendingPlanID(string) error
	DeclinePendingPlanID(string) error
	ApprovePendingDelegation() error
	DeclinePendingDelegation() error
	ResolveInput(uint64, chat.InputIntent, bool) error
	CancelPendingRoute(uint64) error
	AcknowledgeConversation(string) error
	DismissConversation(string) error
	DismissAllConversations() error
	CancelConversation(string) error
	RetryConversation(string) error
	KeepWaitingConversation(string) error
	PromoteConversation(string, bool) error
	FollowUpConversation(string, string) error
	SetPresence(chat.Participant, bool) error
	ScheduleRosterAction(chat.RosterActionType, chat.Participant, time.Time, string) (chat.ScheduledRosterAction, error)
	CancelRosterAction(string) error
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

func (s *Service) authenticatePeer(value HelloRequest, peer PairedPeer) (*Session, error) {
	identity, err := namespacedIdentity(peer.InstanceID, "peer", value.ClientID)
	if err != nil {
		return nil, err
	}
	return &Session{
		Identity: identity, InstanceID: peer.InstanceID, Credential: peer.InstanceID,
		Kind: ClientPeer, Scopes: map[Scope]bool{ScopeObserve: true, ScopeParticipate: true},
	}, nil
}

// NewBridgeSession creates a restricted host-authenticated session for a
// browser or connector gateway. The gateway, not the untrusted client, selects
// the device identity and scopes. Administer scope remains a narrow room-control
// capability; bridge message execution is still confined to read-only asks.
func (s *Service) NewBridgeSession(deviceID, clientID string, scopes []Scope) (*Session, error) {
	deviceID = strings.TrimSpace(deviceID)
	if !validIdentifier(deviceID) {
		return nil, fmt.Errorf("invalid bridge device identity")
	}
	identity, err := namespacedIdentity(s.credentials.InstanceID, "device-"+deviceID, clientID)
	if err != nil {
		return nil, err
	}
	granted := make(map[Scope]bool, len(scopes))
	for _, scope := range scopes {
		if !scope.Valid() {
			return nil, fmt.Errorf("invalid bridge scope %q", scope)
		}
		granted[scope] = true
	}
	if !granted[ScopeObserve] {
		return nil, fmt.Errorf("bridge sessions require observe scope")
	}
	if granted[ScopeAdminister] && !granted[ScopeParticipate] {
		return nil, fmt.Errorf("bridge administer scope requires participate scope")
	}
	return &Session{
		Identity: identity, InstanceID: s.credentials.InstanceID,
		Credential: deviceID, Kind: ClientBridge, Scopes: granted,
	}, nil
}

// Subscribe exposes the sanitized event stream to transport adapters after the
// session has joined its room. A single adapter subscription can assign stable
// replay cursors before fan-out to browser clients.
func (s *Service) Subscribe(session *Session, buffer int) (<-chan Event, func(), error) {
	if session == nil || !session.Has(ScopeObserve) || session.RoomID == "" {
		return nil, nil, fmt.Errorf("an observed joined room is required")
	}
	if buffer < 1 {
		return nil, nil, fmt.Errorf("event buffer must be positive")
	}
	roomState, _ := s.controller.Snapshot()
	if session.RoomID != roomState.ID {
		return nil, nil, fmt.Errorf("joined room is no longer hosted")
	}
	source, stopSource := s.controller.SubscribeEvents(buffer)
	output := make(chan Event, buffer)
	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			stopSource()
		})
	}
	go func() {
		defer close(output)
		defer cancel()
		for {
			select {
			case <-done:
				return
			case value, ok := <-source:
				if !ok {
					return
				}
				event, err := NewEvent(s.InstanceID(), session.RoomID, value, session.Kind == ClientLocal)
				if err != nil {
					return
				}
				select {
				case output <- event:
				case <-done:
					return
				}
			}
		}
	}()
	return output, cancel, nil
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
		view := s.roomView(roomState)
		if provider, ok := s.controller.(interface{ WorkflowActive() bool }); ok {
			view.WorkflowActive = provider.WorkflowActive()
		}
		return succeeded(request, view)
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
	latest := uint64(0)
	if len(messages) > 0 {
		latest = messages[len(messages)-1].Sequence
	}
	through := value.Through
	if through == 0 {
		through = latest
	}
	if through < value.After || through > latest {
		return failed(request, "invalid_request", "history through must be between after and the latest sequence")
	}
	result := make([]MessageView, 0, value.Limit)
	hasMore := false
	nextAfter := value.After
	for _, message := range messages {
		if message.Sequence <= value.After {
			continue
		}
		if message.Sequence > through {
			break
		}
		if len(result) == value.Limit {
			hasMore = true
			break
		}
		result = append(result, messageViewFor(message, session.Kind == ClientLocal))
		nextAfter = message.Sequence
	}
	return succeeded(request, HistoryResult{
		Messages: result, HasMore: hasMore, NextAfter: nextAfter,
		Through: through, LatestSequence: latest,
	})
}

func (s *Service) status(session *Session, request Request) HandleResult {
	if result := s.requireJoined(session, request, ScopeObserve); result != nil {
		return *result
	}
	roomState, messages := s.controller.Snapshot()
	core := s.controller.CoreStatus()
	total, byAgent := chat.CorrectionStatistics(messages)
	operational := room.StatusSnapshot{}
	if provider, ok := s.controller.(interface{ StatusSnapshot() room.StatusSnapshot }); ok {
		operational = provider.StatusSnapshot()
	}
	view := s.roomView(roomState)
	view.WorkflowActive = operational.WorkflowActive
	return succeeded(request, StatusResult{
		Room: view, ActiveCores: append([]chat.Participant(nil), core.Active...),
		Availability: cloneAvailability(core.Availability), Corrections: total, ByAgent: byAgent,
		Operational: operational,
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
	if value.Command == "join" || value.Command == "leave" || strings.HasPrefix(value.Command, "roster.") ||
		value.Command == "continue" || value.Command == "conflict.resolve" || strings.HasPrefix(value.Command, "language.") || strings.HasPrefix(value.Command, "plan.") || strings.HasPrefix(value.Command, "delegation.") || value.Command == "conversation.promote" ||
		(value.Command == "routing.resolve" && value.Intent == chat.InputWork) {
		required = ScopeAdminister
	}
	if result := s.requireJoined(session, request, required); result != nil {
		return *result
	}
	conversationControl := value.Command == "conversation.ack" || value.Command == "conversation.dismiss" || value.Command == "conversation.dismiss_all" || value.Command == "conversation.cancel" || value.Command == "conversation.retry" || value.Command == "conversation.wait" || value.Command == "conversation.followup" || value.Command == "routing.cancel" ||
		(value.Command == "routing.resolve" && value.Intent == chat.InputConversation)
	remoteAdminControl := value.Command == "continue" || value.Command == "conflict.resolve" || strings.HasPrefix(value.Command, "language.") || strings.HasPrefix(value.Command, "plan.") || strings.HasPrefix(value.Command, "delegation.") || value.Command == "conversation.promote" ||
		(value.Command == "routing.resolve" && value.Intent == chat.InputWork)
	remoteAllowed := session.Kind == ClientBridge && (value.Command == "stop" || conversationControl || (session.Has(ScopeAdminister) && remoteAdminControl))
	if session.Kind != ClientLocal && !remoteAllowed {
		return failed(request, "forbidden", "remote guests cannot invoke room-control commands")
	}
	switch value.Command {
	case "continue", "stop", "plan.on", "plan.off", "language.simple", "language.standard", "delegation.adaptive", "delegation.auto", "delegation.ask", "delegation.manual":
	case "conflict.resolve":
		value.DecisionID = strings.TrimSpace(value.DecisionID)
		value.ChoiceID = strings.TrimSpace(value.ChoiceID)
		value.Text = strings.TrimSpace(value.Text)
		roomState, _ := s.controller.Snapshot()
		if value.DecisionID == "" || roomState.Conflict == nil || roomState.Conflict.DecisionID != value.DecisionID {
			return failed(request, "stale_decision", "the pending decision no longer matches this response")
		}
		if value.ChoiceID == "" && value.Text == "" {
			return failed(request, "invalid_request", "conflict.resolve requires choice_id or custom text")
		}
	case "plan.execute", "plan.decline":
		value.PlanID = strings.TrimSpace(value.PlanID)
		roomState, _ := s.controller.Snapshot()
		if value.PlanID == "" || roomState.PendingPlan == nil || roomState.PendingPlan.ID != value.PlanID {
			return failed(request, "stale_plan", "the pending plan no longer matches the confirmed plan")
		}
	case "delegation.run", "delegation.solo":
		value.DelegationID = strings.TrimSpace(value.DelegationID)
		roomState, _ := s.controller.Snapshot()
		if value.DelegationID == "" || roomState.PendingDelegation == nil || roomState.PendingDelegation.ID != value.DelegationID {
			return failed(request, "stale_delegation", "the pending delegation no longer matches the confirmed split")
		}
	case "join", "leave":
		if !value.Participant.ValidAgent() {
			return failed(request, "invalid_request", "a valid participant is required")
		}
	case "roster.schedule":
		if !value.Participant.ValidAgent() || !value.Action.Valid() || value.ExecuteAt.IsZero() {
			return failed(request, "invalid_request", "roster.schedule requires a valid participant, join/leave action, and execute_at")
		}
	case "roster.cancel":
		if strings.TrimSpace(value.ActionID) == "" {
			return failed(request, "invalid_request", "roster.cancel requires action_id")
		}
	case "conversation.ack", "conversation.dismiss", "conversation.cancel", "conversation.retry", "conversation.wait", "conversation.promote":
		if strings.TrimSpace(value.ConversationID) == "" {
			return failed(request, "invalid_request", value.Command+" requires conversation_id")
		}
	case "conversation.dismiss_all":
	case "conversation.followup":
		if strings.TrimSpace(value.ConversationID) == "" || strings.TrimSpace(value.Text) == "" {
			return failed(request, "invalid_request", "conversation.followup requires conversation_id and text")
		}
	case "routing.resolve":
		if value.Sequence == 0 || (value.Intent != chat.InputConversation && value.Intent != chat.InputWork) {
			return failed(request, "invalid_request", "routing.resolve requires sequence and conversation or work intent")
		}
	case "routing.cancel":
		if value.Sequence == 0 {
			return failed(request, "invalid_request", "routing.cancel requires sequence")
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
	case "conflict.resolve":
		resolver, ok := s.controller.(interface {
			ResolveConflict(string, string, string) error
		})
		if !ok {
			err = fmt.Errorf("conflict decisions are unavailable")
		} else {
			err = resolver.ResolveConflict(value.DecisionID, value.ChoiceID, value.Text)
		}
	case "stop":
		s.controller.Stop()
	case "plan.on":
		err = s.controller.SetWorkflowMode(chat.WorkflowPlan)
	case "plan.off":
		err = s.controller.SetWorkflowMode(chat.WorkflowExecute)
	case "language.simple", "language.standard":
		setter, ok := s.controller.(interface {
			SetResponseStyle(chat.ResponseStyle) error
		})
		if !ok {
			err = fmt.Errorf("room language settings are unavailable")
		} else if value.Command == "language.simple" {
			err = setter.SetResponseStyle(chat.ResponseSimple)
		} else {
			err = setter.SetResponseStyle(chat.ResponseStandard)
		}
	case "delegation.adaptive":
		err = s.controller.SetDelegationPolicy(chat.DelegationAdaptive)
	case "delegation.auto":
		err = s.controller.SetDelegationPolicy(chat.DelegationAuto)
	case "delegation.ask":
		err = s.controller.SetDelegationPolicy(chat.DelegationAsk)
	case "delegation.manual":
		err = s.controller.SetDelegationPolicy(chat.DelegationManual)
	case "plan.execute":
		err = s.controller.ExecutePendingPlanID(value.PlanID)
	case "plan.decline":
		err = s.controller.DeclinePendingPlanID(value.PlanID)
	case "delegation.run":
		err = s.controller.ApprovePendingDelegation()
	case "delegation.solo":
		err = s.controller.DeclinePendingDelegation()
	case "join", "leave":
		err = s.controller.SetPresence(value.Participant, value.Command == "join")
	case "roster.schedule":
		var action chat.ScheduledRosterAction
		action, err = s.controller.ScheduleRosterAction(value.Action, value.Participant, value.ExecuteAt, value.Reason)
		if err == nil {
			return succeeded(request, map[string]any{"accepted": true, "message_id": request.Route.MessageID, "roster_action": action})
		}
	case "roster.cancel":
		err = s.controller.CancelRosterAction(value.ActionID)
	case "conversation.ack":
		err = s.controller.AcknowledgeConversation(value.ConversationID)
	case "conversation.dismiss":
		err = s.controller.DismissConversation(value.ConversationID)
	case "conversation.dismiss_all":
		err = s.controller.DismissAllConversations()
	case "conversation.cancel":
		err = s.controller.CancelConversation(value.ConversationID)
	case "conversation.retry":
		err = s.controller.RetryConversation(value.ConversationID)
	case "conversation.wait":
		err = s.controller.KeepWaitingConversation(value.ConversationID)
	case "conversation.promote":
		err = s.controller.PromoteConversation(value.ConversationID, value.Replace)
	case "conversation.followup":
		err = s.controller.FollowUpConversation(value.ConversationID, value.Text)
	case "routing.resolve":
		err = s.controller.ResolveInput(value.Sequence, value.Intent, value.Replace)
	case "routing.cancel":
		err = s.controller.CancelPendingRoute(value.Sequence)
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
	conflict := cloneConflict(value.Conflict)
	pendingPlan := clonePlan(value.PendingPlan)
	pendingDelegation := clonePendingDelegation(value.PendingDelegation)
	return RoomView{
		ID: value.ID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		Moderator: value.Moderator, Members: members, CorePolicy: policy, WorkflowMode: value.WorkflowMode.WithDefault(), DelegationPolicy: value.DelegationPolicy.WithDefault(), StreamMode: value.StreamMode.WithDefault(), ResponseStyle: value.ResponseStyle.WithDefault(),
		CorePromotions: append([]chat.CorePromotion(nil), value.CorePromotions...),
		RosterActions:  cloneRosterActions(value.RosterActions), PendingInputs: len(value.PendingInputs),
		Workflows: cloneWorkflowRecords(value.Workflows), InputResolutions: cloneInputResolutions(value.InputResolutions),
		PendingRoutes: append([]uint64(nil), value.PendingRoutes...), Conversations: cloneConversationViews(value.Conversations), PendingPlan: pendingPlan, PendingDelegation: pendingDelegation, Conflict: conflict,
		Activities: cloneActivities(value.Activities), ManualHolds: cloneManualHolds(value.ManualProviderHolds),
	}
}

func (s *Service) roomView(value chat.Room) RoomView {
	view := roomView(value)
	if provider, ok := s.controller.(interface {
		ParticipantConfigurations() []chat.ParticipantConfiguration
	}); ok {
		view.Participants = append([]chat.ParticipantConfiguration(nil), provider.ParticipantConfigurations()...)
	}
	return view
}

func clonePlan(source *chat.ProposedPlan) *chat.ProposedPlan {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}

func clonePendingDelegation(source *chat.PendingDelegation) *chat.PendingDelegation {
	if source == nil {
		return nil
	}
	result := *source
	result.Tasks = append([]chat.DelegationTask(nil), source.Tasks...)
	result.Joins = append([]chat.Participant(nil), source.Joins...)
	result.Leaves = append([]chat.Participant(nil), source.Leaves...)
	return &result
}

func cloneConflict(source *chat.ConflictState) *chat.ConflictState {
	if source == nil {
		return nil
	}
	result := *source
	result.Reasons = make(map[chat.Participant]string, len(source.Reasons))
	for participant, reason := range source.Reasons {
		result.Reasons[participant] = reason
	}
	result.Choices = append([]chat.DecisionChoice(nil), source.Choices...)
	if source.Resolution != nil {
		resolution := *source.Resolution
		result.Resolution = &resolution
	}
	if source.DraftPlan != nil {
		draft := *source.DraftPlan
		result.DraftPlan = &draft
	}
	if source.TranscriptedAt != nil {
		transcriptedAt := *source.TranscriptedAt
		result.TranscriptedAt = &transcriptedAt
	}
	return &result
}

func cloneWorkflowRecords(source map[string]chat.WorkflowRecord) map[string]chat.WorkflowRecord {
	result := make(map[string]chat.WorkflowRecord, len(source))
	for id, workflow := range source {
		workflow.SourceSequences = append([]uint64(nil), workflow.SourceSequences...)
		workflow.RecoveryActors = append([]chat.Participant(nil), workflow.RecoveryActors...)
		workflow.PendingPlan = clonePlan(workflow.PendingPlan)
		workflow.PendingDelegation = clonePendingDelegation(workflow.PendingDelegation)
		workflow.Conflict = cloneConflict(workflow.Conflict)
		if workflow.DecisionResolution != nil {
			resolution := *workflow.DecisionResolution
			workflow.DecisionResolution = &resolution
		}
		if workflow.CompletedAt != nil {
			completedAt := *workflow.CompletedAt
			workflow.CompletedAt = &completedAt
		}
		if workflow.RecoveryAt != nil {
			recoveryAt := *workflow.RecoveryAt
			workflow.RecoveryAt = &recoveryAt
		}
		result[id] = workflow
	}
	return result
}

func cloneInputResolutions(source map[uint64]chat.InputResolution) map[uint64]chat.InputResolution {
	result := make(map[uint64]chat.InputResolution, len(source))
	for sequence, resolution := range source {
		result[sequence] = resolution
	}
	return result
}

func cloneActivities(source map[chat.Participant]chat.ParticipantActivity) map[chat.Participant]chat.ParticipantActivity {
	result := make(map[chat.Participant]chat.ParticipantActivity, len(source))
	for participant, activity := range source {
		result[participant] = activity
	}
	return result
}

func cloneManualHolds(source map[chat.Participant]chat.ManualProviderHold) map[chat.Participant]chat.ManualProviderHold {
	result := make(map[chat.Participant]chat.ManualProviderHold, len(source))
	for participant, hold := range source {
		result[participant] = hold
	}
	return result
}

func cloneConversationViews(values []chat.ConversationJob) []chat.ConversationJob {
	result := append([]chat.ConversationJob(nil), values...)
	for index := range result {
		result[index].Requested = append([]chat.Participant(nil), result[index].Requested...)
		result[index].Attempts = append([]chat.ConversationAttempt(nil), result[index].Attempts...)
		// Current clients use only scheduler state for the workboard. Keep legacy
		// response-management fields inside the host for migration and audit.
		result[index].Unread = false
		result[index].ActionState = ""
		result[index].InboxCategory = ""
		result[index].AvailableActions = nil
	}
	return result
}

func cloneRosterActions(values []chat.ScheduledRosterAction) []chat.ScheduledRosterAction {
	result := append([]chat.ScheduledRosterAction(nil), values...)
	for index := range result {
		if result[index].CompletedAt != nil {
			completedAt := *result[index].CompletedAt
			result[index].CompletedAt = &completedAt
		}
	}
	return result
}

func messageView(value chat.Message) MessageView {
	return messageViewFor(value, true)
}

func messageViewFor(value chat.Message, local bool) MessageView {
	attachments := make([]AttachmentView, 0, len(value.Attachments))
	for _, attachment := range value.Attachments {
		attachments = append(attachments, AttachmentView{
			ID: attachment.ID, Kind: attachment.Kind, Name: attachment.Name,
			MIMEType: attachment.MIMEType, Size: attachment.Size,
			Width: attachment.Width, Height: attachment.Height,
		})
	}
	var route *chat.RouteMetadata
	if local && value.Route != nil {
		copy := *value.Route
		copy.Hops = append([]string(nil), value.Route.Hops...)
		route = &copy
	}
	text := value.Text
	workflowMode := value.WorkflowMode
	if value.Author == chat.User {
		workflowMode = workflowMode.WithDefault()
	}
	if !local {
		switch value.Kind {
		case chat.MessageTool:
			text = "[tool activity hidden]"
		case chat.MessageStatus:
			text = "[host status hidden]"
		}
	}
	return MessageView{
		ID: value.ID, Sequence: value.Sequence, TurnID: value.TurnID, WorkflowID: value.WorkflowID, DecisionID: value.DecisionID, Author: value.Author, Target: value.Target,
		Kind: value.Kind, WorkflowMode: workflowMode, DelegationPolicy: value.DelegationPolicy, InputIntent: value.InputIntent, IntentConfidence: value.IntentConfidence,
		ConversationID: value.ConversationID, Text: text, Attachments: attachments,
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
		Type: string(value.Type), TurnID: value.TurnID, WorkflowID: value.WorkflowID, Participant: value.Participant,
		Participants: append([]chat.Participant(nil), value.Participants...),
		Wave:         value.Wave, WorkflowMode: value.WorkflowMode, Queued: value.Queued, StreamGap: value.StreamGap,
	}
	if local {
		payload.Text = value.Text
		payload.Role = value.Role
		payload.Task = value.Task
	}
	if local && value.Err != nil {
		payload.Error = value.Err.Error()
	}
	if value.Message != nil {
		message := messageViewFor(*value.Message, local)
		payload.Message = &message
	}
	if value.AgentEvent != nil {
		agentEvent := AgentEventView{Type: string(value.AgentEvent.Type), Agent: value.AgentEvent.Agent}
		if local {
			agentEvent.Text = value.AgentEvent.Text
		}
		if value.AgentEvent.Activity != nil {
			activity := *value.AgentEvent.Activity
			agentEvent.Activity = &activity
		}
		payload.Agent = &agentEvent
	}
	if value.Activity != nil {
		activity := *value.Activity
		payload.Activity = &activity
	}
	if local && value.Turn != nil {
		turn := *value.Turn
		turn.Drafts = append([]string(nil), value.Turn.Drafts...)
		turn.Tools = append([]string(nil), value.Turn.Tools...)
		payload.Turn = &turn
	}
	if value.Plan != nil {
		plan := *value.Plan
		payload.Plan = &plan
	}
	if value.Delegation != nil {
		delegation := *value.Delegation
		delegation.Tasks = append([]chat.DelegationTask(nil), value.Delegation.Tasks...)
		delegation.Joins = append([]chat.Participant(nil), value.Delegation.Joins...)
		delegation.Leaves = append([]chat.Participant(nil), value.Delegation.Leaves...)
		payload.Delegation = &delegation
	}
	if value.Conversation != nil {
		conversation := cloneConversationViews([]chat.ConversationJob{*value.Conversation})[0]
		payload.Conversation = &conversation
	}
	route := Route{}
	if local {
		route = Route{MessageID: id, OriginInstanceID: instanceID, OriginClientID: instanceID + "/host", Hops: []string{instanceID}}
	}
	return Event{
		Version: Version, ID: id, Type: "event", RoomID: roomID,
		Route: route, At: time.Now().UTC(), Payload: payload,
	}, nil
}
