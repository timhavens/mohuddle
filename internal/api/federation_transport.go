package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

func StartFederation(address string, service *Service, audit *AuditLog, identity *FederationIdentity, pairings *PairingStore) (*Server, error) {
	if service == nil || identity == nil || pairings == nil {
		return nil, fmt.Errorf("federation service, identity, and pairing store are required")
	}
	if service.InstanceID() != identity.InstanceID || pairings.instanceID != identity.InstanceID {
		return nil, fmt.Errorf("federation instance identities do not match")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, fmt.Errorf("invalid federation listen address: %w", err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	configuration := &tls.Config{
		Certificates: []tls.Certificate{identity.certificate}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequestClientCert, NextProtos: []string{"mohuddle/1"},
	}
	secured := tls.NewListener(listener, configuration)
	authorize := func(session *Session, connection net.Conn) bool {
		fingerprint, _, err := peerCertificateIdentity(connection)
		return err == nil && pairings.AuthorizeInbound(session.InstanceID, fingerprint)
	}
	return startServer(secured, service, audit, federationHandshake(service, pairings), authorize), nil
}

func federationHandshake(service *Service, pairings *PairingStore) handshakeFunc {
	return func(request Request, connection net.Conn) handshakeResult {
		if request.Version != Version || !validIdentifier(request.ID) {
			return handshakeResult{response: failed(request, "unauthenticated", "the first request must be a valid federation handshake").Response, close: true}
		}
		fingerprint, certificateIdentity, err := peerCertificateIdentity(connection)
		if err != nil {
			return handshakeResult{response: failed(request, "authentication_failed", err.Error()).Response, close: true}
		}
		if request.Type == "pair.accept" {
			value, err := decodePayload[PairAcceptRequest](request)
			if err != nil || value.InstanceID != certificateIdentity {
				return handshakeResult{response: failed(request, "pairing_failed", "pairing identity does not match its certificate").Response, close: true}
			}
			token, err := pairings.accept(value.InvitationID, value.Secret, value.InstanceID, fingerprint)
			if err != nil {
				return handshakeResult{response: failed(request, "pairing_failed", err.Error()).Response, close: true}
			}
			return handshakeResult{
				response: successResponse(request, PairAcceptResult{HostInstanceID: service.InstanceID(), Token: token}),
				close:    true,
			}
		}
		if request.Type != "hello" {
			return handshakeResult{response: failed(request, "unauthenticated", "the first request must be hello or pair.accept").Response, close: true}
		}
		hello, err := decodePayload[HelloRequest](request)
		if err != nil {
			return handshakeResult{response: failed(request, "authentication_failed", err.Error()).Response, close: true}
		}
		peer, ok := pairings.AuthenticateInbound(hello.Token, fingerprint)
		if !ok || peer.InstanceID != certificateIdentity {
			return handshakeResult{response: failed(request, "authentication_failed", "peer is not paired or has been revoked").Response, close: true}
		}
		session, err := service.authenticatePeer(hello, peer)
		if err != nil {
			return handshakeResult{response: failed(request, "authentication_failed", err.Error()).Response, close: true}
		}
		return handshakeResult{session: session, response: helloResponse(request, service, session)}
	}
}

func peerCertificateIdentity(connection net.Conn) (string, string, error) {
	value, ok := connection.(*tls.Conn)
	if !ok {
		return "", "", fmt.Errorf("federation connection is not TLS")
	}
	state := value.ConnectionState()
	if len(state.PeerCertificates) != 1 {
		return "", "", fmt.Errorf("exactly one peer certificate is required")
	}
	if state.NegotiatedProtocol != "mohuddle/1" {
		return "", "", fmt.Errorf("federation protocol negotiation failed")
	}
	certificate := state.PeerCertificates[0]
	if !validIdentifier(certificate.Subject.CommonName) {
		return "", "", fmt.Errorf("peer certificate has an invalid instance identity")
	}
	if time.Now().Before(certificate.NotBefore) || time.Now().After(certificate.NotAfter) {
		return "", "", fmt.Errorf("peer certificate is outside its validity period")
	}
	return certificateFingerprint(certificate.Raw), certificate.Subject.CommonName, nil
}

type PeerClient struct {
	connection    *tls.Conn
	encoder       *json.Encoder
	scanner       *bufio.Scanner
	localInstance string
	identity      string
}

func DialPairedPeer(ctx context.Context, identity *FederationIdentity, pairings *PairingStore, peerInstanceID, clientID string) (*PeerClient, error) {
	if identity == nil || pairings == nil || identity.InstanceID != pairings.instanceID {
		return nil, fmt.Errorf("matching local federation identity and pairing store are required")
	}
	peer, ok := pairings.Outbound(peerInstanceID)
	if !ok {
		return nil, fmt.Errorf("peer %q is not paired for outbound access", peerInstanceID)
	}
	connection, err := dialPinnedTLS(ctx, identity, peer.Address, peer.CertificateFingerprint, peer.InstanceID)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	client := &PeerClient{
		connection: connection, encoder: json.NewEncoder(connection), scanner: scanner,
		localInstance: identity.InstanceID,
	}
	requestID, err := NewID()
	if err != nil {
		connection.Close()
		return nil, err
	}
	response, err := client.request(ctx, requestWithPayload(requestID, "hello", HelloRequest{ClientID: clientID, Token: peer.Token}))
	if err != nil {
		connection.Close()
		return nil, err
	}
	if !response.OK {
		connection.Close()
		return nil, responseError(response)
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		connection.Close()
		return nil, err
	}
	var hello HelloResult
	if err := json.Unmarshal(data, &hello); err != nil {
		connection.Close()
		return nil, err
	}
	if hello.InstanceID != peer.InstanceID || hello.Kind != ClientPeer || hello.Identity == "" {
		connection.Close()
		return nil, fmt.Errorf("paired peer returned an unexpected identity")
	}
	client.identity = hello.Identity
	return client, nil
}

func (c *PeerClient) Close() error { return c.connection.Close() }

func (c *PeerClient) Join(ctx context.Context, roomID string) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	response, err := c.request(ctx, requestWithPayload(id, "room.join", JoinRoomRequest{RoomID: roomID}))
	if err != nil {
		return err
	}
	if !response.OK {
		return responseError(response)
	}
	return nil
}

func (c *PeerClient) Status(ctx context.Context, roomID string) (StatusResult, error) {
	id, err := NewID()
	if err != nil {
		return StatusResult{}, err
	}
	response, err := c.request(ctx, Request{Version: Version, ID: id, Type: "status.get", RoomID: roomID})
	if err != nil {
		return StatusResult{}, err
	}
	if !response.OK {
		return StatusResult{}, responseError(response)
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return StatusResult{}, err
	}
	var result StatusResult
	err = json.Unmarshal(data, &result)
	return result, err
}

func (c *PeerClient) History(ctx context.Context, roomID string, after uint64, limit int) (HistoryResult, error) {
	id, err := NewID()
	if err != nil {
		return HistoryResult{}, err
	}
	request := requestWithPayload(id, "history.get", HistoryRequest{After: after, Limit: limit})
	request.RoomID = roomID
	response, err := c.request(ctx, request)
	if err != nil {
		return HistoryResult{}, err
	}
	if !response.OK {
		return HistoryResult{}, responseError(response)
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return HistoryResult{}, err
	}
	var result HistoryResult
	err = json.Unmarshal(data, &result)
	return result, err
}

func (c *PeerClient) Ask(ctx context.Context, roomID, text string) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	messageID, err := NewID()
	if err != nil {
		return err
	}
	request := requestWithPayload(id, "message.send", SendMessageRequest{Mode: "ask", Text: text})
	request.RoomID = roomID
	request.Route = &Route{MessageID: messageID, OriginInstanceID: c.localInstance, OriginClientID: c.identity}
	response, err := c.request(ctx, request)
	if err != nil {
		return err
	}
	if !response.OK {
		return responseError(response)
	}
	return nil
}

func (c *PeerClient) Subscribe(ctx context.Context, roomID string) (<-chan Event, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	response, err := c.request(ctx, Request{Version: Version, ID: id, Type: "events.subscribe", RoomID: roomID})
	if err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, responseError(response)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.connection.SetDeadline(deadline)
	} else {
		_ = c.connection.SetDeadline(time.Time{})
	}
	stream := make(chan Event, 32)
	go func() {
		defer close(stream)
		stop := context.AfterFunc(ctx, func() { _ = c.connection.Close() })
		defer stop()
		for {
			var event Event
			if !c.scanner.Scan() || json.Unmarshal(c.scanner.Bytes(), &event) != nil {
				return
			}
			if event.Version != Version || event.RoomID != roomID {
				return
			}
			select {
			case stream <- event:
			case <-ctx.Done():
				_ = c.connection.Close()
				return
			}
		}
	}()
	return stream, nil
}

func (c *PeerClient) request(ctx context.Context, request Request) (Response, error) {
	deadline := time.Now().Add(10 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := c.connection.SetDeadline(deadline); err != nil {
		return Response{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.connection.SetDeadline(time.Now()) })
	defer func() {
		if stop() {
			_ = c.connection.SetDeadline(time.Time{})
		}
	}()
	if err := c.encoder.Encode(request); err != nil {
		return Response{}, err
	}
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("peer closed the connection before responding")
	}
	var response Response
	if err := json.Unmarshal(c.scanner.Bytes(), &response); err != nil {
		return Response{}, fmt.Errorf("decode peer response: %w", err)
	}
	if response.Version != Version || response.ID != request.ID {
		return Response{}, fmt.Errorf("peer returned a mismatched response")
	}
	return response, nil
}

func responseError(response Response) error {
	if response.Error == nil {
		return fmt.Errorf("peer request failed")
	}
	return fmt.Errorf("peer request failed (%s): %s", response.Error.Code, response.Error.Message)
}
