package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/room"
)

type Server struct {
	service  *Service
	audit    *AuditLog
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	close    sync.Once
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	closing  bool
}

func StartLocal(socketPath string, service *Service, audit *AuditLog) (*Server, error) {
	if service == nil {
		return nil, fmt.Errorf("API service is required")
	}
	listener, err := listenLocal(socketPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{service: service, audit: audit, listener: listener, ctx: ctx, cancel: cancel, conns: make(map[net.Conn]struct{})}
	server.wg.Add(1)
	go server.accept()
	return server, nil
}

func (s *Server) Addr() string { return s.listener.Addr().String() }

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.connMu.Lock()
		if s.closing {
			s.connMu.Unlock()
			_ = connection.Close()
			return
		}
		s.conns[connection] = struct{}{}
		s.wg.Add(1)
		s.connMu.Unlock()
		go func() {
			defer s.wg.Done()
			s.serve(connection)
		}()
	}
}

func (s *Server) serve(connection net.Conn) {
	defer func() {
		connection.Close()
		s.connMu.Lock()
		delete(s.conns, connection)
		s.connMu.Unlock()
	}()
	connectionID, _ := NewID()
	remote := connection.RemoteAddr().String()
	identity := ""
	_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Remote: remote, Action: "connect", Allowed: true})
	defer func() {
		_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Identity: identity, Remote: remote, Action: "disconnect", Allowed: true})
	}()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	encoder := json.NewEncoder(connection)
	var session *Session
	for scanner.Scan() {
		var request Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			response := Response{Version: Version, OK: false, Error: &ProtocolError{Code: "invalid_json", Message: "request is not valid JSON"}}
			_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Identity: identity, Remote: remote, Action: "invalid_json", Error: err.Error()})
			if !writeJSON(connection, encoder, response) {
				return
			}
			continue
		}
		if session == nil {
			if request.Version != Version || request.Type != "hello" || !validIdentifier(request.ID) {
				response := failed(request, "unauthenticated", "the first request must be a valid hello").Response
				_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Remote: remote, Action: "hello", RequestID: request.ID, Error: response.Error.Message})
				writeJSON(connection, encoder, response)
				return
			}
			hello, err := decodePayload[HelloRequest](request)
			if err == nil {
				session, err = s.service.Authenticate(hello)
			}
			if err != nil {
				response := failed(request, "authentication_failed", err.Error()).Response
				_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Remote: remote, Action: "hello", RequestID: request.ID, Error: err.Error()})
				writeJSON(connection, encoder, response)
				return
			}
			identity = session.Identity
			scopes := make([]Scope, 0, len(session.Scopes))
			for scope := range session.Scopes {
				scopes = append(scopes, scope)
			}
			sort.Slice(scopes, func(i, j int) bool { return scopes[i] < scopes[j] })
			response := successResponse(request, HelloResult{Identity: session.Identity, InstanceID: s.service.InstanceID(), Kind: session.Kind, Scopes: scopes})
			_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Identity: identity, Remote: remote, Action: "hello", RequestID: request.ID, Allowed: true})
			if !writeJSON(connection, encoder, response) {
				return
			}
			continue
		}
		result := s.service.Handle(s.ctx, session, request)
		record := AuditRecord{ConnectionID: connectionID, Identity: identity, Remote: remote, Action: request.Type, RequestID: request.ID, Allowed: result.Response.OK}
		if result.Response.Error != nil {
			record.Error = result.Response.Error.Message
		}
		_ = s.audit.Append(record)
		if result.Subscribe {
			stream, cancel := s.service.controller.SubscribeEvents(512)
			if !writeJSON(connection, encoder, result.Response) {
				cancel()
				return
			}
			s.stream(connection, encoder, session, stream, cancel)
			return
		}
		if !writeJSON(connection, encoder, result.Response) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		_ = s.audit.Append(AuditRecord{ConnectionID: connectionID, Identity: identity, Remote: remote, Action: "read_error", Error: err.Error()})
	}
}

func (s *Server) stream(connection net.Conn, encoder *json.Encoder, session *Session, stream <-chan room.Event, cancel func()) {
	defer cancel()
	for {
		select {
		case <-s.ctx.Done():
			return
		case value, ok := <-stream:
			if !ok {
				return
			}
			event, err := NewEvent(s.service.InstanceID(), session.RoomID, value, session.Kind == ClientLocal)
			if err != nil {
				return
			}
			if !writeJSON(connection, encoder, event) {
				return
			}
		}
	}
}

func writeJSON(connection net.Conn, encoder *json.Encoder, value any) bool {
	_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return encoder.Encode(value) == nil
}

func (s *Server) Close() error {
	var err error
	s.close.Do(func() {
		s.cancel()
		s.connMu.Lock()
		s.closing = true
		s.connMu.Unlock()
		err = s.listener.Close()
		s.connMu.Lock()
		for connection := range s.conns {
			connection.Close()
		}
		s.connMu.Unlock()
		s.wg.Wait()
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
