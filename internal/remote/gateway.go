// Package remote provides the explicitly enabled, browser-facing MoHuddle
// gateway. It owns HTTP/WebSocket transport security and device authentication;
// room semantics remain in the shared api.Service.
package remote

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/remote/device"
	"github.com/timhavens/mohuddle/internal/remote/events"
)

const (
	sessionCookie         = "mohuddle_remote_session"
	maximumBodyBytes      = 64 << 10
	streamBuffer          = 32
	syncHistoryLimit      = 100
	websocketWriteTimeout = 10 * time.Second
)

type Config struct {
	ListenAddress      string
	Origin             string
	TLSCertFile        string
	TLSKeyFile         string
	RoomID             string
	Service            *api.Service
	Devices            *device.Store
	Audit              *api.AuditLog
	Assets             fs.FS
	SessionIdleTTL     time.Duration
	SessionAbsoluteTTL time.Duration
}

type Gateway struct {
	listener net.Listener
	server   *http.Server
	service  *api.Service
	devices  *device.Store
	audit    *api.AuditLog
	hub      *events.Hub
	origin   string
	host     string
	secure   bool
	lifetime context.Context

	cancelSource       func()
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	closeOnce          sync.Once
	limits             *rateLimits
	socketMu           sync.Mutex
	sockets            map[*websocket.Conn]struct{}
	sessionIdleTTL     time.Duration
	sessionAbsoluteTTL time.Duration
}

type pairRequest struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

type pairResult struct {
	DeviceID          string         `json:"device_id"`
	Name              string         `json:"name"`
	RoomID            string         `json:"room_id"`
	Scopes            []device.Scope `json:"scopes"`
	PermissionCeiling string         `json:"permission_ceiling"`
}

type challengeRequest struct {
	DeviceID string `json:"device_id"`
}

type challengeResult struct {
	ChallengeID string    `json:"challenge_id"`
	Payload     string    `json:"payload"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type sessionRequest struct {
	DeviceID    string `json:"device_id"`
	ChallengeID string `json:"challenge_id"`
	Signature   string `json:"signature"`
}

type sessionResult struct {
	Identity          string         `json:"identity"`
	DeviceID          string         `json:"device_id"`
	RoomID            string         `json:"room_id"`
	Scopes            []device.Scope `json:"scopes"`
	PermissionCeiling string         `json:"permission_ceiling"`
	CSRFToken         string         `json:"csrf_token"`
	ExpiresAt         time.Time      `json:"expires_at"`
}

type syncFrame struct {
	Type    string            `json:"type"`
	Cursor  events.Cursor     `json:"cursor"`
	History api.HistoryResult `json:"history"`
	Room    api.RoomView      `json:"room"`
	Gap     *gapView          `json:"gap,omitempty"`
}

type eventFrame struct {
	Type   string        `json:"type"`
	Cursor events.Cursor `json:"cursor"`
	Event  api.Event     `json:"event"`
}

type gapFrame struct {
	Type   string        `json:"type"`
	Cursor events.Cursor `json:"cursor"`
	Gap    gapView       `json:"gap"`
}

type gapView struct {
	Reason          events.GapReason `json:"reason"`
	Requested       events.Cursor    `json:"requested"`
	OldestAvailable events.Cursor    `json:"oldest_available"`
	Current         events.Cursor    `json:"current"`
	HistoryAfter    uint64           `json:"history_after"`
}

type errorEnvelope struct {
	Error api.ProtocolError `json:"error"`
}

func Start(config Config) (*Gateway, error) {
	if strings.TrimSpace(config.ListenAddress) == "" {
		return nil, fmt.Errorf("remote gateway listen address is required")
	}
	if config.Service == nil || config.Devices == nil || config.Assets == nil {
		return nil, fmt.Errorf("remote gateway service, device store, and assets are required")
	}
	if strings.TrimSpace(config.RoomID) == "" {
		return nil, fmt.Errorf("remote gateway room identity is required")
	}
	if (config.TLSCertFile == "") != (config.TLSKeyFile == "") {
		return nil, fmt.Errorf("remote TLS certificate and key must be configured together")
	}
	host, _, err := net.SplitHostPort(config.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("remote listen address: %w", err)
	}
	loopback := isLoopbackHost(host)
	tlsEnabled := config.TLSCertFile != ""
	if !loopback && !tlsEnabled {
		return nil, fmt.Errorf("non-loopback remote access requires TLS")
	}
	var tlsConfig *tls.Config
	if tlsEnabled {
		certificate, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load remote TLS identity: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return nil, err
	}
	origin, err := normalizeOrigin(config.Origin, listener.Addr().String(), loopback, tlsEnabled)
	if err != nil {
		listener.Close()
		return nil, err
	}
	parsedOrigin, _ := url.Parse(origin)

	eventSession, err := config.Service.NewBridgeSession("remote-gateway", "event-source", []api.Scope{api.ScopeObserve})
	if err != nil {
		listener.Close()
		return nil, err
	}
	eventSession.RoomID = config.RoomID
	source, cancelSource, err := config.Service.Subscribe(eventSession, streamBuffer)
	if err != nil {
		listener.Close()
		return nil, err
	}
	latest, err := historyHighWater(config.Service, eventSession, config.RoomID)
	if err != nil {
		cancelSource()
		listener.Close()
		return nil, err
	}
	hub, err := events.New(events.Options{InitialMessageSequence: latest})
	if err != nil {
		cancelSource()
		listener.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	gateway := &Gateway{
		listener: listener, service: config.Service, devices: config.Devices,
		audit: config.Audit, hub: hub, origin: origin, host: parsedOrigin.Host,
		secure: parsedOrigin.Scheme == "https", lifetime: ctx,
		cancelSource: cancelSource, cancel: cancel,
		limits: newRateLimits(), sockets: make(map[*websocket.Conn]struct{}),
		sessionIdleTTL: config.SessionIdleTTL, sessionAbsoluteTTL: config.SessionAbsoluteTTL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pair", gateway.handlePair)
	mux.HandleFunc("POST /api/v1/challenge", gateway.handleChallenge)
	mux.HandleFunc("POST /api/v1/session", gateway.handleSession)
	mux.HandleFunc("POST /api/v1/request", gateway.handleRequest)
	mux.HandleFunc("GET /api/v1/events", gateway.handleEvents)
	mux.Handle("/", gateway.staticHandler(config.Assets))
	gateway.server = &http.Server{
		Handler:           gateway.securityHeaders(mux),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	gateway.wg.Add(2)
	go gateway.pumpEvents(ctx, source)
	go func() {
		defer gateway.wg.Done()
		if tlsEnabled {
			_ = gateway.server.ServeTLS(listener, "", "")
		} else {
			_ = gateway.server.Serve(listener)
		}
	}()
	return gateway, nil
}

func (g *Gateway) Addr() string   { return g.listener.Addr().String() }
func (g *Gateway) Origin() string { return g.origin }

func (g *Gateway) Close() error {
	var closeErr error
	g.closeOnce.Do(func() {
		g.cancel()
		g.cancelSource()
		g.closeSockets()
		g.hub.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeErr = g.server.Shutdown(ctx)
		if errors.Is(closeErr, context.DeadlineExceeded) {
			closeErr = g.server.Close()
		}
		g.wg.Wait()
	})
	return closeErr
}

func (g *Gateway) pumpEvents(ctx context.Context, source <-chan api.Event) {
	defer g.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case value, ok := <-source:
			if !ok {
				return
			}
			if value.Payload.StreamGap > 0 {
				_, _ = g.hub.Invalidate(events.GapUpstreamOverflow)
				continue
			}
			_, _ = g.hub.Publish(value)
		}
	}
}

func (g *Gateway) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if g.secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) staticHandler(assets fs.FS) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		files.ServeHTTP(w, r)
	})
}

func (g *Gateway) handlePair(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeOrigin(w, r) || !g.limits.allow(clientIP(r), "pair", 5, time.Minute) {
		if g.authorizeOriginOnly(r) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many pairing attempts")
		}
		return
	}
	var value pairRequest
	if !decodeJSON(w, r, &value) {
		return
	}
	publicKey, err := base64.StdEncoding.DecodeString(value.PublicKey)
	if err != nil {
		g.auditFailure(r, "remote.pair", "", "invalid public key")
		writeError(w, http.StatusBadRequest, "invalid_request", "public key is not valid base64")
		return
	}
	grant, err := g.devices.Pair(value.Code, publicKey)
	if err != nil {
		g.auditFailure(r, "remote.pair", "", err.Error())
		writeError(w, http.StatusUnauthorized, "pairing_failed", "pairing code is invalid or expired")
		return
	}
	g.auditDevice(r, "remote.pair", grant.ID, "", grant.RoomID, grant.Scopes, true, "")
	writeJSON(w, http.StatusOK, pairResult{
		DeviceID: grant.ID, Name: grant.Name, RoomID: grant.RoomID,
		Scopes: grant.Scopes, PermissionCeiling: string(grant.PermissionCeiling),
	})
}

func (g *Gateway) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeOrigin(w, r) || !g.limits.allow(clientIP(r), "challenge", 20, time.Minute) {
		if g.authorizeOriginOnly(r) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many authentication attempts")
		}
		return
	}
	var value challengeRequest
	if !decodeJSON(w, r, &value) {
		return
	}
	challenge, err := g.devices.NewChallenge(value.DeviceID, 0)
	if err != nil {
		g.auditFailure(r, "remote.challenge", value.DeviceID, err.Error())
		writeError(w, http.StatusForbidden, "authentication_failed", "device is unavailable")
		return
	}
	g.auditDevice(r, "remote.challenge", value.DeviceID, "", challenge.RoomID, nil, true, "")
	writeJSON(w, http.StatusOK, challengeResult{ChallengeID: challenge.ID, Payload: challenge.Payload, ExpiresAt: challenge.ExpiresAt})
}

func (g *Gateway) handleSession(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeOrigin(w, r) || !g.limits.allow(clientIP(r), "session", 20, time.Minute) {
		if g.authorizeOriginOnly(r) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many authentication attempts")
		}
		return
	}
	var value sessionRequest
	if !decodeJSON(w, r, &value) {
		return
	}
	signature, err := base64.StdEncoding.DecodeString(value.Signature)
	if err != nil {
		g.auditFailure(r, "remote.session", value.DeviceID, "invalid signature encoding")
		writeError(w, http.StatusBadRequest, "invalid_request", "signature is not valid base64")
		return
	}
	credentials, err := g.devices.CompleteChallenge(value.DeviceID, value.ChallengeID, signature, g.sessionIdleTTL, g.sessionAbsoluteTTL)
	if err != nil {
		g.auditFailure(r, "remote.session", value.DeviceID, err.Error())
		writeError(w, http.StatusUnauthorized, "authentication_failed", "device proof is invalid or expired")
		return
	}
	session := credentials.Session
	apiSession, err := g.bridgeSession(session)
	if err != nil {
		writeError(w, http.StatusForbidden, "authentication_failed", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: credentials.Token, Path: "/api/v1",
		HttpOnly: true, Secure: g.secure, SameSite: http.SameSiteStrictMode,
		Expires: session.AbsoluteExpiresAt, MaxAge: int(time.Until(session.AbsoluteExpiresAt).Seconds()),
	})
	g.auditDevice(r, "remote.session", session.DeviceID, session.ID, session.RoomID, session.Scopes, true, "")
	writeJSON(w, http.StatusOK, sessionResult{
		Identity: apiSession.Identity, DeviceID: session.DeviceID, RoomID: session.RoomID,
		Scopes: session.Scopes, PermissionCeiling: string(session.PermissionCeiling),
		CSRFToken: credentials.CSRFToken, ExpiresAt: session.AbsoluteExpiresAt,
	})
}

func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeOrigin(w, r) {
		return
	}
	session, token, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if !g.devices.VerifyCSRF(session.ID, r.Header.Get("X-MoHuddle-CSRF")) {
		g.auditDevice(r, "remote.request", session.DeviceID, session.ID, session.RoomID, session.Scopes, false, "CSRF validation failed")
		writeError(w, http.StatusForbidden, "csrf_failed", "request authentication failed")
		return
	}
	_ = token
	var request api.Request
	if !decodeJSON(w, r, &request) {
		return
	}
	r.Header.Set("X-Request-ID", request.ID)
	if !allowedRemoteRequest(request.Type) {
		g.auditDevice(r, request.Type, session.DeviceID, session.ID, session.RoomID, session.Scopes, false, "request type is not remotely exposed")
		writeJSON(w, http.StatusForbidden, api.Response{Version: api.Version, ID: request.ID, OK: false, Error: &api.ProtocolError{Code: "forbidden", Message: "request type is not remotely exposed"}})
		return
	}
	apiSession, err := g.bridgeSession(session)
	if err != nil {
		writeError(w, http.StatusForbidden, "authentication_failed", err.Error())
		return
	}
	request.RoomID = session.RoomID
	if request.Type == "message.send" {
		operation := sha256.Sum256([]byte(session.DeviceID + "\x00" + request.ID))
		messageID := fmt.Sprintf("%x", operation[:])
		request.Route = &api.Route{MessageID: messageID, OriginInstanceID: apiSession.InstanceID, OriginClientID: apiSession.Identity}
	} else {
		request.Route = nil
	}
	result := g.service.Handle(r.Context(), apiSession, request)
	errorText := ""
	if result.Response.Error != nil {
		errorText = result.Response.Error.Message
	}
	g.auditDevice(r, request.Type, session.DeviceID, session.ID, session.RoomID, session.Scopes, result.Response.OK, errorText)
	writeJSON(w, http.StatusOK, result.Response)
}

func (g *Gateway) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeOrigin(w, r) {
		return
	}
	session, _, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("room_id") != session.RoomID {
		writeError(w, http.StatusForbidden, "room_not_found", "event stream does not target the paired room")
		return
	}
	after, err := cursorFromQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	subscription, err := g.hub.Subscribe(after, streamBuffer)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "stream_unavailable", err.Error())
		return
	}
	defer subscription.Cancel()
	apiSession, err := g.bridgeSession(session)
	if err != nil {
		writeError(w, http.StatusForbidden, "authentication_failed", err.Error())
		return
	}
	historyAfter := after.MessageSequence
	if historyAfter > subscription.Current.MessageSequence {
		historyAfter = 0
	}
	history, err := readHistoryPage(g.service, apiSession, session.RoomID, historyAfter, subscription.Current.MessageSequence, syncHistoryLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "history_failed", err.Error())
		return
	}
	roomValue, err := roomSnapshot(g.service, apiSession, session.RoomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot_failed", err.Error())
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	if !g.registerSocket(connection) {
		connection.CloseNow()
		return
	}
	defer g.unregisterSocket(connection)
	defer connection.CloseNow()
	closeDetail := ""
	defer func() {
		g.auditDevice(r, "remote.events.close", session.DeviceID, session.ID, session.RoomID, session.Scopes, true, closeDetail)
	}()
	syncCursor := subscription.Current
	if subscription.Gap == nil && len(subscription.Replay) > 0 {
		syncCursor = after
	}
	if err := g.writeFrame(connection, syncFrame{Type: "sync", Cursor: syncCursor, History: history, Room: roomValue, Gap: remoteGap(subscription.Gap)}); err != nil {
		return
	}
	for _, record := range subscription.Replay {
		if err := g.writeFrame(connection, eventFrame{Type: "event", Cursor: record.Cursor, Event: record.Event}); err != nil {
			return
		}
	}
	g.auditDevice(r, "remote.events", session.DeviceID, session.ID, session.RoomID, session.Scopes, true, "")
	deadline := session.IdleExpiresAt
	if session.AbsoluteExpiresAt.Before(deadline) {
		deadline = session.AbsoluteExpiresAt
	}
	expires := time.NewTimer(time.Until(deadline))
	defer expires.Stop()
	for {
		select {
		case <-g.lifetime.Done():
			closeDetail = "gateway stopped"
			return
		case <-session.Done:
			code := websocket.StatusCode(4003)
			reason := "device revoked"
			closeDetail = reason
			if grant, exists := g.devices.Grant(session.DeviceID); exists && grant.Active() {
				code = websocket.StatusCode(4001)
				reason = "device authorization changed"
				closeDetail = reason
			}
			_ = connection.Close(code, reason)
			return
		case <-expires.C:
			closeDetail = "session expired"
			_ = connection.Close(websocket.StatusCode(4001), "session expired")
			return
		case delivery, open := <-subscription.Events:
			if !open {
				return
			}
			if delivery.Record != nil {
				if err := g.writeFrame(connection, eventFrame{Type: "event", Cursor: delivery.Record.Cursor, Event: delivery.Record.Event}); err != nil {
					return
				}
			} else if delivery.Gap != nil {
				if err := g.writeFrame(connection, gapFrame{Type: "gap", Cursor: delivery.Gap.Current, Gap: *remoteGap(delivery.Gap)}); err != nil {
					return
				}
			}
		}
	}
}

func (g *Gateway) registerSocket(connection *websocket.Conn) bool {
	g.socketMu.Lock()
	defer g.socketMu.Unlock()
	if g.lifetime.Err() != nil {
		return false
	}
	g.sockets[connection] = struct{}{}
	return true
}

func (g *Gateway) unregisterSocket(connection *websocket.Conn) {
	g.socketMu.Lock()
	delete(g.sockets, connection)
	g.socketMu.Unlock()
}

func (g *Gateway) closeSockets() {
	g.socketMu.Lock()
	connections := make([]*websocket.Conn, 0, len(g.sockets))
	for connection := range g.sockets {
		connections = append(connections, connection)
		delete(g.sockets, connection)
	}
	g.socketMu.Unlock()
	for _, connection := range connections {
		connection.CloseNow()
	}
}

func (g *Gateway) writeFrame(connection *websocket.Conn, value any) error {
	ctx, cancel := context.WithTimeout(g.lifetime, websocketWriteTimeout)
	defer cancel()
	return wsjson.Write(ctx, connection, value)
}

func (g *Gateway) bridgeSession(value device.Session) (*api.Session, error) {
	scopes := make([]api.Scope, 0, len(value.Scopes))
	for _, scope := range value.Scopes {
		switch scope {
		case device.ScopeObserve:
			scopes = append(scopes, api.ScopeObserve)
		case device.ScopeParticipate:
			scopes = append(scopes, api.ScopeParticipate)
		default:
			return nil, fmt.Errorf("device has an unsupported scope")
		}
	}
	session, err := g.service.NewBridgeSession(value.DeviceID, "session-"+value.ID, scopes)
	if err != nil {
		return nil, err
	}
	session.RoomID = value.RoomID
	return session, nil
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (device.Session, string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "session_expired", "remote session is required")
		return device.Session{}, "", false
	}
	session, err := g.devices.Authenticate(cookie.Value)
	if err != nil {
		g.auditFailure(r, "remote.authenticate", "", err.Error())
		writeError(w, http.StatusUnauthorized, "session_expired", "remote session is invalid or expired")
		return device.Session{}, "", false
	}
	return session, cookie.Value, true
}

func (g *Gateway) authorizeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if g.authorizeOriginOnly(r) {
		return true
	}
	g.auditFailure(r, "remote.origin", "", "origin or host rejected")
	writeError(w, http.StatusForbidden, "origin_rejected", "request origin is not authorized")
	return false
}

func (g *Gateway) authorizeOriginOnly(r *http.Request) bool {
	return constantString(r.Host, g.host) && constantString(r.Header.Get("Origin"), g.origin)
}

func (g *Gateway) auditFailure(r *http.Request, action, deviceID, message string) {
	g.auditDevice(r, action, deviceID, "", "", nil, false, message)
}

func (g *Gateway) auditDevice(r *http.Request, action, deviceID, sessionID, roomID string, scopes []device.Scope, allowed bool, message string) {
	values := make([]api.Scope, 0, len(scopes))
	for _, scope := range scopes {
		values = append(values, api.Scope(scope))
	}
	identity := ""
	if deviceID != "" {
		identity = "device:" + deviceID
	}
	_ = g.audit.Append(api.AuditRecord{
		ConnectionID: requestConnectionID(r), Identity: identity, Remote: r.RemoteAddr, Action: action,
		RequestID: r.Header.Get("X-Request-ID"), DeviceID: deviceID,
		SessionID: sessionID, RoomID: roomID, Scopes: values,
		Permission: string(device.CeilingReadOnly), Allowed: allowed, Error: message,
	})
}

func allowedRemoteRequest(value string) bool {
	switch value {
	case "room.join", "room.get", "history.get", "status.get", "message.send":
		return true
	default:
		return false
	}
}

func remoteGap(value *events.Gap) *gapView {
	if value == nil {
		return nil
	}
	return &gapView{
		Reason: value.Reason, Requested: value.Requested, OldestAvailable: value.OldestAvailable,
		Current: value.Current, HistoryAfter: value.Requested.MessageSequence,
	}
}

func historyHighWater(service *api.Service, session *api.Session, roomID string) (uint64, error) {
	value, err := readHistoryPage(service, session, roomID, 0, 0, 1)
	if err != nil {
		return 0, err
	}
	return value.LatestSequence, nil
}

func readHistoryPage(service *api.Service, session *api.Session, roomID string, after, through uint64, limit int) (api.HistoryResult, error) {
	requestID, err := api.NewID()
	if err != nil {
		return api.HistoryResult{}, err
	}
	payload, _ := json.Marshal(api.HistoryRequest{After: after, Through: through, Limit: limit})
	response := service.Handle(context.Background(), session, api.Request{Version: api.Version, ID: requestID, Type: "history.get", RoomID: roomID, Payload: payload}).Response
	if !response.OK {
		return api.HistoryResult{}, fmt.Errorf("history: %s", response.Error.Message)
	}
	page, ok := response.Result.(api.HistoryResult)
	if !ok {
		return api.HistoryResult{}, fmt.Errorf("history returned an unexpected result")
	}
	return page, nil
}

func roomSnapshot(service *api.Service, session *api.Session, roomID string) (api.RoomView, error) {
	requestID, err := api.NewID()
	if err != nil {
		return api.RoomView{}, err
	}
	response := service.Handle(context.Background(), session, api.Request{Version: api.Version, ID: requestID, Type: "room.get", RoomID: roomID}).Response
	if !response.OK {
		return api.RoomView{}, fmt.Errorf("room snapshot: %s", response.Error.Message)
	}
	value, ok := response.Result.(api.RoomView)
	if !ok {
		return api.RoomView{}, fmt.Errorf("room snapshot returned an unexpected result")
	}
	return value, nil
}

func cursorFromQuery(values url.Values) (events.Cursor, error) {
	eventSequence, err := parseCursorValue(values.Get("after_event"))
	if err != nil {
		return events.Cursor{}, fmt.Errorf("invalid event cursor")
	}
	messageSequence, err := parseCursorValue(values.Get("after_message"))
	if err != nil {
		return events.Cursor{}, fmt.Errorf("invalid message cursor")
	}
	return events.Cursor{BootID: values.Get("boot_id"), EventSequence: eventSequence, MessageSequence: messageSequence}, nil
}

func parseCursorValue(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func normalizeOrigin(configured, address string, loopback, secure bool) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		if !loopback {
			return "", fmt.Errorf("non-loopback remote access requires an exact --remote-origin")
		}
		scheme := "http"
		if secure {
			scheme = "https"
		}
		configured = scheme + "://" + address
	}
	value, err := url.Parse(configured)
	if err != nil || value.Host == "" || value.Path != "" || value.RawQuery != "" || value.Fragment != "" || value.User != nil {
		return "", fmt.Errorf("remote origin must be an exact scheme and host")
	}
	if value.Scheme != "https" && !(loopback && value.Scheme == "http") {
		return "", fmt.Errorf("remote origin must use HTTPS except on loopback")
	}
	return strings.TrimSuffix(value.String(), "/"), nil
}

func isLoopbackHost(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), "[]")
	if strings.EqualFold(value, "localhost") {
		return true
	}
	ip := net.ParseIP(value)
	return ip != nil && ip.IsLoopback()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestConnectionID(r *http.Request) string {
	value := r.Context().Value(connectionIDKey{})
	if id, ok := value.(string); ok {
		return id
	}
	id, _ := api.NewID()
	return id
}

type connectionIDKey struct{}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	reader := http.MaxBytesReader(w, r.Body, maximumBodyBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: api.ProtocolError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func constantString(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

type rateLimits struct {
	mu     sync.Mutex
	values map[string]rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimits() *rateLimits { return &rateLimits{values: make(map[string]rateWindow)} }

func (r *rateLimits) allow(client, action string, maximum int, duration time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	key := client + "\x00" + action
	if len(r.values) >= 4096 {
		for candidate, window := range r.values {
			if now.Sub(window.start) >= duration {
				delete(r.values, candidate)
			}
		}
		if _, exists := r.values[key]; !exists && len(r.values) >= 4096 {
			return false
		}
	}
	value := r.values[key]
	if value.start.IsZero() || now.Sub(value.start) >= duration {
		value = rateWindow{start: now}
	}
	value.count++
	r.values[key] = value
	return value.count <= maximum
}
