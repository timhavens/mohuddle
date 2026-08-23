package api

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	FederationPairVersion  = "mohuddle.pair.v1"
	federationIdentityFile = "federation_identity.json"
	federationPairingsFile = "federation_pairings.json"
	pairingPrefix          = "mohuddle-pair-v1:"
)

type FederationIdentity struct {
	InstanceID     string `json:"instance_id"`
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`

	certificate tls.Certificate
	fingerprint string
}

type PairInvitation struct {
	Version                string    `json:"version"`
	ID                     string    `json:"id"`
	HostInstanceID         string    `json:"host_instance_id"`
	Address                string    `json:"address"`
	CertificateFingerprint string    `json:"certificate_fingerprint"`
	Secret                 string    `json:"secret"`
	ExpiresAt              time.Time `json:"expires_at"`
}

type PendingPairInvitation struct {
	ID         string    `json:"id"`
	Address    string    `json:"address"`
	SecretHash string    `json:"secret_hash"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type PairedPeer struct {
	InstanceID             string    `json:"instance_id"`
	Address                string    `json:"address,omitempty"`
	CertificateFingerprint string    `json:"certificate_fingerprint"`
	Token                  string    `json:"token"`
	CreatedAt              time.Time `json:"created_at"`
}

type PairingState struct {
	Version  string                  `json:"version"`
	Pending  []PendingPairInvitation `json:"pending,omitempty"`
	Inbound  []PairedPeer            `json:"inbound,omitempty"`
	Outbound []PairedPeer            `json:"outbound,omitempty"`
}

type PairingStore struct {
	path       string
	instanceID string
	mu         sync.Mutex
	state      PairingState
}

type PairAcceptRequest struct {
	InvitationID string `json:"invitation_id"`
	Secret       string `json:"secret"`
	InstanceID   string `json:"instance_id"`
}

type PairAcceptResult struct {
	HostInstanceID string `json:"host_instance_id"`
	Token          string `json:"token"`
}

func FederationIdentityPath(stateRoot string) string {
	return filepath.Join(stateRoot, federationIdentityFile)
}

func FederationPairingsPath(stateRoot string) string {
	return filepath.Join(stateRoot, federationPairingsFile)
}

func LoadOrCreateFederationIdentity(path, instanceID string) (*FederationIdentity, error) {
	if !validIdentifier(instanceID) {
		return nil, fmt.Errorf("invalid federation instance identity")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("federation identity path must be a regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var value FederationIdentity
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode federation identity: %w", err)
		}
		if err := value.prepare(instanceID); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
		return &value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	value, err := newFederationIdentity(instanceID)
	if err != nil {
		return nil, err
	}
	if err := createPrivateJSON(path, value); errors.Is(err, os.ErrExist) {
		return LoadOrCreateFederationIdentity(path, instanceID)
	} else if err != nil {
		return nil, err
	}
	return value, nil
}

func newFederationIdentity(instanceID string) (*FederationIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: instanceID},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	value := &FederationIdentity{
		InstanceID:     instanceID,
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
	if err := value.prepare(instanceID); err != nil {
		return nil, err
	}
	return value, nil
}

func (i *FederationIdentity) prepare(instanceID string) error {
	if i.InstanceID != instanceID {
		return fmt.Errorf("federation identity belongs to %q, expected %q", i.InstanceID, instanceID)
	}
	certificate, err := tls.X509KeyPair([]byte(i.CertificatePEM), []byte(i.PrivateKeyPEM))
	if err != nil {
		return fmt.Errorf("load federation key pair: %w", err)
	}
	if len(certificate.Certificate) != 1 {
		return fmt.Errorf("federation identity must contain exactly one certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse federation certificate: %w", err)
	}
	if leaf.Subject.CommonName != instanceID {
		return fmt.Errorf("federation certificate identity mismatch")
	}
	if time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return fmt.Errorf("federation certificate is outside its validity period")
	}
	i.certificate = certificate
	i.fingerprint = certificateFingerprint(certificate.Certificate[0])
	return nil
}

func (i *FederationIdentity) Fingerprint() string { return i.fingerprint }

func LoadPairingStore(path, instanceID string) (*PairingStore, error) {
	if !validIdentifier(instanceID) {
		return nil, fmt.Errorf("invalid pairing-store instance identity")
	}
	value, err := loadPairingState(path, instanceID)
	if err != nil {
		return nil, err
	}
	return &PairingStore{path: path, instanceID: instanceID, state: value}, nil
}

func loadPairingState(path, instanceID string) (PairingState, error) {
	value := PairingState{Version: FederationPairVersion}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return PairingState{}, fmt.Errorf("federation pairing path must be a regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return PairingState{}, err
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return PairingState{}, fmt.Errorf("decode federation pairings: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return PairingState{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return PairingState{}, err
	}
	if value.Version != FederationPairVersion {
		return PairingState{}, fmt.Errorf("unsupported federation pairing version %q", value.Version)
	}
	if err := validatePairingState(value, instanceID); err != nil {
		return PairingState{}, err
	}
	return value, nil
}

func validatePairingState(value PairingState, instanceID string) error {
	seenPending := make(map[string]bool)
	seenInbound := make(map[string]bool)
	seenOutbound := make(map[string]bool)
	for _, pending := range value.Pending {
		if !validIdentifier(pending.ID) || !validFingerprint(pending.SecretHash) || pending.CreatedAt.IsZero() || !pending.ExpiresAt.After(pending.CreatedAt) || seenPending[pending.ID] {
			return fmt.Errorf("invalid or duplicate pending federation invitation")
		}
		if err := validateFederationAddress(pending.Address); err != nil {
			return err
		}
		seenPending[pending.ID] = true
	}
	for _, group := range []struct {
		name string
		list []PairedPeer
		seen map[string]bool
	}{{"inbound", value.Inbound, seenInbound}, {"outbound", value.Outbound, seenOutbound}} {
		for _, peer := range group.list {
			if !validIdentifier(peer.InstanceID) || peer.InstanceID == instanceID || group.seen[peer.InstanceID] || len(peer.Token) < 32 || !validFingerprint(peer.CertificateFingerprint) {
				return fmt.Errorf("invalid or duplicate %s federation peer %q", group.name, peer.InstanceID)
			}
			if group.name == "outbound" {
				if err := validateFederationAddress(peer.Address); err != nil {
					return err
				}
			}
			group.seen[peer.InstanceID] = true
		}
	}
	return nil
}

func (s *PairingStore) CreateInvitation(address string, identity *FederationIdentity, ttl time.Duration) (PairInvitation, error) {
	if identity == nil || identity.InstanceID != s.instanceID {
		return PairInvitation{}, fmt.Errorf("matching federation identity is required")
	}
	if err := validateFederationAddress(address); err != nil {
		return PairInvitation{}, err
	}
	address = strings.TrimSpace(address)
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return PairInvitation{}, fmt.Errorf("pairing invitation lifetime must be between 1 minute and 24 hours")
	}
	id, err := NewID()
	if err != nil {
		return PairInvitation{}, err
	}
	secret, err := randomToken()
	if err != nil {
		return PairInvitation{}, err
	}
	now := time.Now().UTC()
	invitation := PairInvitation{
		Version: FederationPairVersion, ID: id, HostInstanceID: s.instanceID,
		Address: address, CertificateFingerprint: identity.Fingerprint(), Secret: secret,
		ExpiresAt: now.Add(ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := acquirePairingFileLock(s.path)
	if err != nil {
		return PairInvitation{}, err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return PairInvitation{}, err
	}
	next := clonePairingState(s.state)
	next.Pending = prunePending(next.Pending, now)
	next.Pending = append(next.Pending, PendingPairInvitation{
		ID: id, Address: address, SecretHash: hashSecret(secret), ExpiresAt: invitation.ExpiresAt, CreatedAt: now,
	})
	if err := s.saveLocked(next); err != nil {
		return PairInvitation{}, err
	}
	return invitation, nil
}

func (s *PairingStore) accept(invitationID, secret, peerInstanceID, fingerprint string) (string, error) {
	if !validIdentifier(peerInstanceID) || peerInstanceID == s.instanceID || !validFingerprint(fingerprint) {
		return "", fmt.Errorf("invalid peer identity")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := acquirePairingFileLock(s.path)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return "", err
	}
	next := clonePairingState(s.state)
	index := -1
	for candidate, pending := range next.Pending {
		if pending.ID == invitationID {
			index = candidate
			if now.After(pending.ExpiresAt) || subtle.ConstantTimeCompare([]byte(pending.SecretHash), []byte(hashSecret(secret))) != 1 {
				return "", fmt.Errorf("pairing invitation is invalid or expired")
			}
			break
		}
	}
	if index < 0 {
		return "", fmt.Errorf("pairing invitation is invalid or expired")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	next.Pending = append(next.Pending[:index], next.Pending[index+1:]...)
	next.Inbound = upsertPeer(next.Inbound, PairedPeer{
		InstanceID: peerInstanceID, CertificateFingerprint: fingerprint, Token: token, CreatedAt: now,
	})
	if err := s.saveLocked(next); err != nil {
		return "", err
	}
	return token, nil
}

func (s *PairingStore) SaveOutbound(invitation PairInvitation, token string) error {
	if err := invitation.Validate(); err != nil {
		return err
	}
	if invitation.HostInstanceID == s.instanceID || len(token) < 32 {
		return fmt.Errorf("invalid outbound pairing")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := acquirePairingFileLock(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	next := clonePairingState(s.state)
	next.Outbound = upsertPeer(next.Outbound, PairedPeer{
		InstanceID: invitation.HostInstanceID, Address: invitation.Address,
		CertificateFingerprint: invitation.CertificateFingerprint, Token: token, CreatedAt: time.Now().UTC(),
	})
	return s.saveLocked(next)
}

func (s *PairingStore) AuthenticateInbound(token, fingerprint string) (PairedPeer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return PairedPeer{}, false
	}
	for _, peer := range s.state.Inbound {
		if subtle.ConstantTimeCompare([]byte(peer.Token), []byte(token)) == 1 && subtle.ConstantTimeCompare([]byte(peer.CertificateFingerprint), []byte(fingerprint)) == 1 {
			return peer, true
		}
	}
	return PairedPeer{}, false
}

func (s *PairingStore) AuthorizeInbound(instanceID, fingerprint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return false
	}
	for _, peer := range s.state.Inbound {
		if peer.InstanceID == instanceID && subtle.ConstantTimeCompare([]byte(peer.CertificateFingerprint), []byte(fingerprint)) == 1 {
			return true
		}
	}
	return false
}

func (s *PairingStore) Outbound(instanceID string) (PairedPeer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return PairedPeer{}, false
	}
	for _, peer := range s.state.Outbound {
		if peer.InstanceID == instanceID {
			return peer, true
		}
	}
	return PairedPeer{}, false
}

func (s *PairingStore) List() PairingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	value := clonePairingState(s.state)
	value.Inbound = sortedPeers(value.Inbound)
	value.Outbound = sortedPeers(value.Outbound)
	return value
}

func (s *PairingStore) Revoke(instanceID string) error {
	if !validIdentifier(instanceID) {
		return fmt.Errorf("invalid peer identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := acquirePairingFileLock(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	next := clonePairingState(s.state)
	next.Inbound = removePeer(next.Inbound, instanceID)
	next.Outbound = removePeer(next.Outbound, instanceID)
	return s.saveLocked(next)
}

func (s *PairingStore) saveLocked(next PairingState) error {
	if err := validatePairingState(next, s.instanceID); err != nil {
		return err
	}
	if err := replacePrivateJSON(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *PairingStore) reloadLocked() error {
	value, err := loadPairingState(s.path, s.instanceID)
	if err != nil {
		return err
	}
	s.state = value
	return nil
}

func (i PairInvitation) Validate() error {
	if i.Version != FederationPairVersion || !validIdentifier(i.ID) || !validIdentifier(i.HostInstanceID) || len(i.Secret) < 32 || !validFingerprint(i.CertificateFingerprint) || time.Now().After(i.ExpiresAt) {
		return fmt.Errorf("pairing invitation is invalid or expired")
	}
	return validateFederationAddress(i.Address)
}

func (i PairInvitation) Encode() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(i)
	if err != nil {
		return "", err
	}
	return pairingPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodePairInvitation(value string) (PairInvitation, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, pairingPrefix) {
		return PairInvitation{}, fmt.Errorf("invalid pairing invitation prefix")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, pairingPrefix))
	if err != nil {
		return PairInvitation{}, fmt.Errorf("decode pairing invitation: %w", err)
	}
	var invitation PairInvitation
	if err := json.Unmarshal(data, &invitation); err != nil {
		return PairInvitation{}, fmt.Errorf("decode pairing invitation: %w", err)
	}
	if err := invitation.Validate(); err != nil {
		return PairInvitation{}, err
	}
	return invitation, nil
}

func replacePrivateJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".federation-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func clonePairingState(value PairingState) PairingState {
	value.Pending = append([]PendingPairInvitation(nil), value.Pending...)
	value.Inbound = append([]PairedPeer(nil), value.Inbound...)
	value.Outbound = append([]PairedPeer(nil), value.Outbound...)
	return value
}

func prunePending(values []PendingPairInvitation, now time.Time) []PendingPairInvitation {
	result := values[:0]
	for _, value := range values {
		if now.Before(value.ExpiresAt) {
			result = append(result, value)
		}
	}
	return result
}

func upsertPeer(values []PairedPeer, peer PairedPeer) []PairedPeer {
	for index := range values {
		if values[index].InstanceID == peer.InstanceID {
			values[index] = peer
			return values
		}
	}
	return append(values, peer)
}

func removePeer(values []PairedPeer, instanceID string) []PairedPeer {
	result := values[:0]
	for _, peer := range values {
		if peer.InstanceID != instanceID {
			result = append(result, peer)
		}
	}
	return result
}

func hashSecret(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func certificateFingerprint(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func validFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateFederationAddress(value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || strings.TrimSpace(host) == "" || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("federation address must be an explicit host:port")
	}
	return nil
}

func sortedPeers(values []PairedPeer) []PairedPeer {
	result := append([]PairedPeer(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i].InstanceID < result[j].InstanceID })
	return result
}

func pinnedTLSConfig(identity *FederationIdentity, fingerprint, instanceID string) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{identity.certificate}, MinVersion: tls.VersionTLS13,
		InsecureSkipVerify: true, // Verification is the explicit invitation/peer pin below.
		NextProtos:         []string{"mohuddle/1"},
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 || subtle.ConstantTimeCompare([]byte(certificateFingerprint(state.PeerCertificates[0].Raw)), []byte(fingerprint)) != 1 {
				return fmt.Errorf("federation certificate pin mismatch")
			}
			certificate := state.PeerCertificates[0]
			if certificate.Subject.CommonName != instanceID {
				return fmt.Errorf("federation certificate identity mismatch")
			}
			if time.Now().Before(certificate.NotBefore) || time.Now().After(certificate.NotAfter) {
				return fmt.Errorf("federation certificate is outside its validity period")
			}
			return nil
		},
	}
}

func dialPinnedTLS(ctx context.Context, identity *FederationIdentity, address, fingerprint, instanceID string) (*tls.Conn, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: pinnedTLSConfig(identity, fingerprint, instanceID)}
	plain, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	connection, ok := plain.(*tls.Conn)
	if !ok {
		plain.Close()
		return nil, fmt.Errorf("federation dial did not produce a TLS connection")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	return connection, nil
}

func AcceptPairInvitation(ctx context.Context, identity *FederationIdentity, pairings *PairingStore, invitation PairInvitation) error {
	if identity == nil || pairings == nil || identity.InstanceID != pairings.instanceID {
		return fmt.Errorf("matching local federation identity and pairing store are required")
	}
	if err := invitation.Validate(); err != nil {
		return err
	}
	if invitation.HostInstanceID == identity.InstanceID {
		return fmt.Errorf("an instance cannot pair with itself")
	}
	connection, err := dialPinnedTLS(ctx, identity, invitation.Address, invitation.CertificateFingerprint, invitation.HostInstanceID)
	if err != nil {
		return fmt.Errorf("connect to pairing host: %w", err)
	}
	defer connection.Close()
	requestID, err := NewID()
	if err != nil {
		return err
	}
	request := requestWithPayload(requestID, "pair.accept", PairAcceptRequest{
		InvitationID: invitation.ID, Secret: invitation.Secret, InstanceID: identity.InstanceID,
	})
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return err
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("pairing host closed the connection before responding")
	}
	var response Response
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return fmt.Errorf("decode pairing response: %w", err)
	}
	if !response.OK {
		if response.Error != nil {
			return fmt.Errorf("pairing rejected: %s", response.Error.Message)
		}
		return fmt.Errorf("pairing rejected")
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return err
	}
	var result PairAcceptResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if result.HostInstanceID != invitation.HostInstanceID || len(result.Token) < 32 {
		return fmt.Errorf("pairing host returned an invalid identity or credential")
	}
	return pairings.SaveOutbound(invitation, result.Token)
}

func requestWithPayload(id, kind string, payload any) Request {
	data, _ := json.Marshal(payload)
	return Request{Version: Version, ID: id, Type: kind, Payload: data}
}
