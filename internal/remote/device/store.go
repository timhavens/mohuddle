// Package device owns the durable identities and short-lived authentication
// state used by remote MoHuddle devices. It deliberately has no dependency on
// the room or API packages so the gateway can enforce this boundary before a
// request reaches the shared protocol service.
package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	stateVersion = "mohuddle.remote-devices.v1"

	ScopeObserve     Scope = "observe"
	ScopeParticipate Scope = "participate"
	ScopeAdmin       Scope = "admin"

	CeilingReadOnly PermissionCeiling = "read-only"

	defaultInvitationTTL = 15 * time.Minute
	maximumInvitationTTL = 24 * time.Hour
	defaultChallengeTTL  = 2 * time.Minute
	maximumChallengeTTL  = 10 * time.Minute
	defaultIdleTTL       = 15 * time.Minute
	defaultAbsoluteTTL   = 8 * time.Hour
	maximumAbsoluteTTL   = 24 * time.Hour
)

type Scope string

type PermissionCeiling string

// Grant is the persistent, room-bound authority assigned by the trusted host.
// PublicKeySPKI is the canonical DER SubjectPublicKeyInfo for an ECDSA P-256
// device key. Revoked grants remain in the store as durable audit state.
type Grant struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	RoomID            string            `json:"room_id"`
	PublicKeySPKI     []byte            `json:"public_key_spki"`
	Scopes            []Scope           `json:"scopes"`
	PermissionCeiling PermissionCeiling `json:"permission_ceiling"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	RevokedAt         *time.Time        `json:"revoked_at,omitempty"`
}

func (g Grant) Active() bool { return g.RevokedAt == nil }

type invitationRecord struct {
	ID                string            `json:"id"`
	CodeHash          string            `json:"code_hash"`
	Name              string            `json:"name"`
	RoomID            string            `json:"room_id"`
	Scopes            []Scope           `json:"scopes"`
	PermissionCeiling PermissionCeiling `json:"permission_ceiling"`
	CreatedAt         time.Time         `json:"created_at"`
	ExpiresAt         time.Time         `json:"expires_at"`
}

type persistentState struct {
	Version     string             `json:"version"`
	Invitations []invitationRecord `json:"invitations,omitempty"`
	Grants      []Grant            `json:"grants,omitempty"`
}

// Invitation is safe to show as text or encode into a QR code. The code is
// returned once and only its hash is persisted.
type Invitation struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Challenge contains the exact UTF-8 payload a browser signs with WebCrypto
// ECDSA/P-256/SHA-256. A challenge is consumed by the first completion attempt.
type Challenge struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	RoomID    string    `json:"room_id"`
	Payload   string    `json:"payload"`
	ExpiresAt time.Time `json:"expires_at"`
}

type challengeRecord struct {
	Challenge Challenge
}

// Session is an immutable snapshot of a live device session. Done closes when
// the device is revoked, the session expires, or Close is called.
type Session struct {
	ID                string
	DeviceID          string
	RoomID            string
	Scopes            []Scope
	PermissionCeiling PermissionCeiling
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	Done              <-chan struct{}
}

type SessionCredentials struct {
	Token     string
	CSRFToken string
	Session   Session
}

func (s Session) Has(scope Scope) bool {
	for _, candidate := range s.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type sessionRecord struct {
	id                string
	deviceID          string
	roomID            string
	scopes            []Scope
	ceiling           PermissionCeiling
	tokenHash         [sha256.Size]byte
	csrfHash          [sha256.Size]byte
	createdAt         time.Time
	lastSeenAt        time.Time
	idleTTL           time.Duration
	idleExpiresAt     time.Time
	absoluteExpiresAt time.Time
	done              chan struct{}
	closed            bool
}

// Store serializes durable updates and owns all in-memory challenge/session
// state. Multiple Store values must not write the same path concurrently.
type Store struct {
	path string

	mu         sync.Mutex
	state      persistentState
	challenges map[string]challengeRecord
	sessions   map[string]*sessionRecord
	now        func() time.Time
	random     io.Reader
	closed     bool
}

func Open(path string) (*Store, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, fmt.Errorf("device store path is required")
	}
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return nil, err
	}
	state, err := loadState(path)
	if err != nil {
		return nil, err
	}
	return &Store{
		path: path, state: state, challenges: make(map[string]challengeRecord),
		sessions: make(map[string]*sessionRecord), now: func() time.Time { return time.Now().UTC() }, random: rand.Reader,
	}, nil
}

func (s *Store) CreateInvitation(roomID, name string, scopes []Scope, ttl time.Duration) (Invitation, error) {
	roomID = strings.TrimSpace(roomID)
	name = strings.TrimSpace(name)
	validScopes, err := normalizeScopes(scopes)
	if err != nil {
		return Invitation{}, err
	}
	if !validIdentifier(roomID) {
		return Invitation{}, fmt.Errorf("invalid room identity")
	}
	if err := validateName(name); err != nil {
		return Invitation{}, err
	}
	if ttl == 0 {
		ttl = defaultInvitationTTL
	}
	if ttl < time.Second || ttl > maximumInvitationTTL {
		return Invitation{}, fmt.Errorf("invitation lifetime must be between 1 second and 24 hours")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return Invitation{}, err
	}
	now := s.now().UTC()
	id, err := randomIdentifier(s.random, 16)
	if err != nil {
		return Invitation{}, err
	}
	code, err := humanCode(s.random)
	if err != nil {
		return Invitation{}, err
	}
	record := invitationRecord{
		ID: id, CodeHash: hashCode(code), Name: name, RoomID: roomID,
		Scopes: validScopes, PermissionCeiling: CeilingReadOnly,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	next := cloneState(s.state)
	next.Invitations = pruneInvitations(next.Invitations, now)
	next.Invitations = append(next.Invitations, record)
	if err := saveState(s.path, next); err != nil {
		return Invitation{}, err
	}
	s.state = next
	return Invitation{ID: id, Code: code, ExpiresAt: record.ExpiresAt}, nil
}

// Pair consumes a one-time code and publishes the durable device grant in the
// same atomic state replacement. Invalid completion attempts consume no code;
// successful codes can never be reused.
func (s *Store) Pair(code string, publicKeySPKI []byte) (Grant, error) {
	canonical, _, err := canonicalP256SPKI(publicKeySPKI)
	if err != nil {
		return Grant{}, err
	}
	wantedHash := hashCode(code)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return Grant{}, err
	}
	now := s.now().UTC()
	next := cloneState(s.state)
	index := -1
	for candidate := range next.Invitations {
		record := next.Invitations[candidate]
		if now.Before(record.ExpiresAt) && constantHexEqual(record.CodeHash, wantedHash) {
			index = candidate
		}
	}
	if index < 0 {
		return Grant{}, fmt.Errorf("pairing code is invalid or expired")
	}
	fingerprint := sha256.Sum256(canonical)
	for _, existing := range next.Grants {
		existingFingerprint := sha256.Sum256(existing.PublicKeySPKI)
		if existing.Active() && subtle.ConstantTimeCompare(existingFingerprint[:], fingerprint[:]) == 1 {
			return Grant{}, fmt.Errorf("device key is already paired")
		}
	}
	record := next.Invitations[index]
	id, err := randomIdentifier(s.random, 16)
	if err != nil {
		return Grant{}, err
	}
	grant := Grant{
		ID: id, Name: record.Name, RoomID: record.RoomID, PublicKeySPKI: canonical,
		Scopes: append([]Scope(nil), record.Scopes...), PermissionCeiling: CeilingReadOnly,
		CreatedAt: now, UpdatedAt: now,
	}
	next.Invitations = append(next.Invitations[:index], next.Invitations[index+1:]...)
	next.Invitations = pruneInvitations(next.Invitations, now)
	next.Grants = append(next.Grants, grant)
	if err := saveState(s.path, next); err != nil {
		return Grant{}, err
	}
	s.state = next
	return cloneGrant(grant), nil
}

func (s *Store) List() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Grant, len(s.state.Grants))
	for index, grant := range s.state.Grants {
		result[index] = cloneGrant(grant)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func (s *Store) Grant(deviceID string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := grantIndex(s.state.Grants, strings.TrimSpace(deviceID))
	if index < 0 {
		return Grant{}, false
	}
	return cloneGrant(s.state.Grants[index]), true
}

func (s *Store) Revoke(deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return err
	}
	now := s.now().UTC()
	next := cloneState(s.state)
	index := grantIndex(next.Grants, deviceID)
	if index < 0 {
		return fmt.Errorf("device grant not found")
	}
	if next.Grants[index].RevokedAt == nil {
		revokedAt := now
		next.Grants[index].RevokedAt = &revokedAt
		next.Grants[index].UpdatedAt = now
		if err := saveState(s.path, next); err != nil {
			return err
		}
		s.state = next
	}
	s.invalidateDeviceSessionsLocked(deviceID)
	for id, challenge := range s.challenges {
		if challenge.Challenge.DeviceID == deviceID {
			delete(s.challenges, id)
		}
	}
	return nil
}

// SetScopes changes a live grant's host-selected authority atomically and
// invalidates all challenges and sessions so the new scope cannot be bypassed
// through credentials issued under the old grant.
func (s *Store) SetScopes(deviceID string, scopes []Scope) (Grant, error) {
	deviceID = strings.TrimSpace(deviceID)
	normalized, err := normalizeScopes(scopes)
	if err != nil {
		return Grant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return Grant{}, err
	}
	next := cloneState(s.state)
	index := grantIndex(next.Grants, deviceID)
	if index < 0 || !next.Grants[index].Active() {
		return Grant{}, fmt.Errorf("device grant is unavailable")
	}
	next.Grants[index].Scopes = append([]Scope(nil), normalized...)
	next.Grants[index].UpdatedAt = s.now().UTC()
	if err := saveState(s.path, next); err != nil {
		return Grant{}, err
	}
	s.state = next
	s.invalidateDeviceSessionsLocked(deviceID)
	for id, challenge := range s.challenges {
		if challenge.Challenge.DeviceID == deviceID {
			delete(s.challenges, id)
		}
	}
	return cloneGrant(next.Grants[index]), nil
}

func (s *Store) NewChallenge(deviceID string, ttl time.Duration) (Challenge, error) {
	deviceID = strings.TrimSpace(deviceID)
	if ttl == 0 {
		ttl = defaultChallengeTTL
	}
	if ttl < time.Second || ttl > maximumChallengeTTL {
		return Challenge{}, fmt.Errorf("challenge lifetime must be between 1 second and 10 minutes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return Challenge{}, err
	}
	now := s.now().UTC()
	s.pruneEphemeralLocked(now)
	index := grantIndex(s.state.Grants, deviceID)
	if index < 0 || !s.state.Grants[index].Active() {
		return Challenge{}, fmt.Errorf("device grant is unavailable")
	}
	id, err := randomIdentifier(s.random, 16)
	if err != nil {
		return Challenge{}, err
	}
	nonceBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonceBytes); err != nil {
		return Challenge{}, err
	}
	expiresAt := now.Add(ttl)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	payload := strings.Join([]string{
		"mohuddle-device-session-v1", id, deviceID, s.state.Grants[index].RoomID,
		nonce, expiresAt.Format(time.RFC3339Nano),
	}, "\n")
	challenge := Challenge{ID: id, DeviceID: deviceID, RoomID: s.state.Grants[index].RoomID, Payload: payload, ExpiresAt: expiresAt}
	s.challenges[id] = challengeRecord{Challenge: challenge}
	return challenge, nil
}

// CompleteChallenge verifies the 64-byte IEEE P1363 signature emitted by
// WebCrypto for ECDSA P-256/SHA-256 and mints an in-memory session.
func (s *Store) CompleteChallenge(deviceID, challengeID string, signature []byte, idleTTL, absoluteTTL time.Duration) (SessionCredentials, error) {
	if idleTTL == 0 {
		idleTTL = defaultIdleTTL
	}
	if absoluteTTL == 0 {
		absoluteTTL = defaultAbsoluteTTL
	}
	if idleTTL < time.Second || absoluteTTL < time.Second || idleTTL > absoluteTTL || absoluteTTL > maximumAbsoluteTTL {
		return SessionCredentials{}, fmt.Errorf("session lifetime is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return SessionCredentials{}, err
	}
	now := s.now().UTC()
	s.pruneEphemeralLocked(now)
	record, ok := s.challenges[challengeID]
	if ok {
		delete(s.challenges, challengeID)
	}
	if !ok || record.Challenge.DeviceID != deviceID || !now.Before(record.Challenge.ExpiresAt) {
		return SessionCredentials{}, fmt.Errorf("challenge is invalid or expired")
	}
	index := grantIndex(s.state.Grants, deviceID)
	if index < 0 || !s.state.Grants[index].Active() {
		return SessionCredentials{}, fmt.Errorf("device grant is unavailable")
	}
	publicKeyBytes := s.state.Grants[index].PublicKeySPKI
	_, publicKey, err := canonicalP256SPKI(publicKeyBytes)
	if err != nil {
		return SessionCredentials{}, fmt.Errorf("stored device key is invalid: %w", err)
	}
	if !verifyRawP256(publicKey, []byte(record.Challenge.Payload), signature) {
		return SessionCredentials{}, fmt.Errorf("device signature is invalid")
	}
	token, err := randomSecret(s.random)
	if err != nil {
		return SessionCredentials{}, err
	}
	csrf, err := randomSecret(s.random)
	if err != nil {
		return SessionCredentials{}, err
	}
	sessionID, err := randomIdentifier(s.random, 16)
	if err != nil {
		return SessionCredentials{}, err
	}
	absoluteExpiresAt := now.Add(absoluteTTL)
	idleExpiresAt := minTime(now.Add(idleTTL), absoluteExpiresAt)
	session := &sessionRecord{
		id: sessionID, deviceID: deviceID, roomID: s.state.Grants[index].RoomID,
		scopes: append([]Scope(nil), s.state.Grants[index].Scopes...), ceiling: CeilingReadOnly,
		tokenHash: sha256.Sum256([]byte(token)), csrfHash: sha256.Sum256([]byte(csrf)),
		createdAt: now, lastSeenAt: now, idleTTL: idleTTL,
		idleExpiresAt: idleExpiresAt, absoluteExpiresAt: absoluteExpiresAt, done: make(chan struct{}),
	}
	s.sessions[sessionID] = session
	return SessionCredentials{Token: token, CSRFToken: csrf, Session: session.snapshot()}, nil
}

// Authenticate performs a full constant-time scan over live token hashes. It
// refreshes the idle deadline without extending the absolute deadline.
func (s *Store) Authenticate(token string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	s.pruneEphemeralLocked(now)
	wanted := sha256.Sum256([]byte(token))
	var matched *sessionRecord
	for _, candidate := range s.sessions {
		if subtle.ConstantTimeCompare(candidate.tokenHash[:], wanted[:]) == 1 {
			matched = candidate
		}
	}
	if matched == nil {
		return Session{}, fmt.Errorf("session is invalid or expired")
	}
	grant := grantIndex(s.state.Grants, matched.deviceID)
	if grant < 0 || !s.state.Grants[grant].Active() {
		s.closeSessionLocked(matched)
		delete(s.sessions, matched.id)
		return Session{}, fmt.Errorf("device grant is unavailable")
	}
	matched.lastSeenAt = now
	matched.idleExpiresAt = minTime(now.Add(matched.idleTTL), matched.absoluteExpiresAt)
	return matched.snapshot(), nil
}

func (s *Store) VerifyCSRF(sessionID, csrfToken string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneEphemeralLocked(now)
	record, ok := s.sessions[sessionID]
	wanted := sha256.Sum256([]byte(csrfToken))
	var stored [sha256.Size]byte
	if ok {
		stored = record.csrfHash
	}
	return subtle.ConstantTimeCompare(stored[:], wanted[:]) == 1 && ok
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for _, session := range s.sessions {
		s.closeSessionLocked(session)
	}
	clear(s.sessions)
	clear(s.challenges)
}

func (s *Store) requireOpenLocked() error {
	if s.closed {
		return fmt.Errorf("device store is closed")
	}
	return nil
}

func (s *Store) pruneEphemeralLocked(now time.Time) {
	for id, challenge := range s.challenges {
		if !now.Before(challenge.Challenge.ExpiresAt) {
			delete(s.challenges, id)
		}
	}
	for id, session := range s.sessions {
		if !now.Before(session.idleExpiresAt) || !now.Before(session.absoluteExpiresAt) {
			s.closeSessionLocked(session)
			delete(s.sessions, id)
		}
	}
}

func (s *Store) invalidateDeviceSessionsLocked(deviceID string) {
	for id, session := range s.sessions {
		if session.deviceID == deviceID {
			s.closeSessionLocked(session)
			delete(s.sessions, id)
		}
	}
}

func (s *Store) closeSessionLocked(session *sessionRecord) {
	if !session.closed {
		close(session.done)
		session.closed = true
	}
}

func (s *sessionRecord) snapshot() Session {
	return Session{
		ID: s.id, DeviceID: s.deviceID, RoomID: s.roomID,
		Scopes: append([]Scope(nil), s.scopes...), PermissionCeiling: s.ceiling,
		CreatedAt: s.createdAt, LastSeenAt: s.lastSeenAt, IdleExpiresAt: s.idleExpiresAt,
		AbsoluteExpiresAt: s.absoluteExpiresAt, Done: s.done,
	}
}

func loadState(path string) (persistentState, error) {
	state := persistentState{Version: stateVersion}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return persistentState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return persistentState{}, fmt.Errorf("device state path must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return persistentState{}, fmt.Errorf("device state file must have mode 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return persistentState{}, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return persistentState{}, fmt.Errorf("decode device state: %w", err)
	}
	if err := validateState(state); err != nil {
		return persistentState{}, err
	}
	return state, nil
}

func saveState(path string, state persistentState) error {
	if err := validateState(state); err != nil {
		return err
	}
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("device state path must be a regular file")
		}
		if info.Mode().Perm() != 0o600 {
			return fmt.Errorf("device state file must have mode 0600")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".remote-devices-*.tmp")
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
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func ensurePrivateParent(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(absolute) {
		return fmt.Errorf("device state parent must not traverse symbolic links")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("device state parent must be a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("device state parent must have mode 0700")
	}
	return nil
}

func validateState(state persistentState) error {
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported device state version %q", state.Version)
	}
	invitationIDs := make(map[string]bool, len(state.Invitations))
	for _, invitation := range state.Invitations {
		if !validIdentifier(invitation.ID) || invitationIDs[invitation.ID] || !validHash(invitation.CodeHash) ||
			!validIdentifier(invitation.RoomID) || validateName(invitation.Name) != nil ||
			invitation.PermissionCeiling != CeilingReadOnly || invitation.CreatedAt.IsZero() || !invitation.ExpiresAt.After(invitation.CreatedAt) {
			return fmt.Errorf("invalid or duplicate device invitation")
		}
		if _, err := normalizeScopes(invitation.Scopes); err != nil {
			return fmt.Errorf("invalid device invitation scopes: %w", err)
		}
		invitationIDs[invitation.ID] = true
	}
	grantIDs := make(map[string]bool, len(state.Grants))
	for _, grant := range state.Grants {
		if !validIdentifier(grant.ID) || grantIDs[grant.ID] || !validIdentifier(grant.RoomID) ||
			validateName(grant.Name) != nil || grant.PermissionCeiling != CeilingReadOnly || grant.CreatedAt.IsZero() || grant.UpdatedAt.Before(grant.CreatedAt) {
			return fmt.Errorf("invalid or duplicate device grant")
		}
		if _, err := normalizeScopes(grant.Scopes); err != nil {
			return fmt.Errorf("invalid device grant scopes: %w", err)
		}
		if _, _, err := canonicalP256SPKI(grant.PublicKeySPKI); err != nil {
			return fmt.Errorf("invalid device grant key: %w", err)
		}
		if grant.RevokedAt != nil && grant.RevokedAt.Before(grant.CreatedAt) {
			return fmt.Errorf("invalid device revocation timestamp")
		}
		grantIDs[grant.ID] = true
	}
	return nil
}

func normalizeScopes(scopes []Scope) ([]Scope, error) {
	seenObserve := false
	seenParticipate := false
	seenAdmin := false
	for _, scope := range scopes {
		switch scope {
		case ScopeObserve:
			if seenObserve {
				return nil, fmt.Errorf("duplicate observe scope")
			}
			seenObserve = true
		case ScopeParticipate:
			if seenParticipate {
				return nil, fmt.Errorf("duplicate participate scope")
			}
			seenParticipate = true
		case ScopeAdmin:
			if seenAdmin {
				return nil, fmt.Errorf("duplicate admin scope")
			}
			seenAdmin = true
		default:
			return nil, fmt.Errorf("invalid device scope %q", scope)
		}
	}
	if !seenObserve || (seenAdmin && !seenParticipate) {
		return nil, fmt.Errorf("device scopes must be observe, observe plus participate, or observe plus participate plus admin")
	}
	result := []Scope{ScopeObserve}
	if seenParticipate {
		result = append(result, ScopeParticipate)
	}
	if seenAdmin {
		result = append(result, ScopeAdmin)
	}
	return result, nil
}

func validateName(name string) error {
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return fmt.Errorf("device name must be 1 to 128 UTF-8 bytes")
	}
	for _, value := range name {
		if unicode.IsControl(value) {
			return fmt.Errorf("device name contains control characters")
		}
	}
	return nil
}

func canonicalP256SPKI(value []byte) ([]byte, *ecdsa.PublicKey, error) {
	parsed, err := x509.ParsePKIXPublicKey(value)
	if err != nil {
		return nil, nil, fmt.Errorf("parse device public key: %w", err)
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() || publicKey.X == nil || publicKey.Y == nil || !publicKey.Curve.IsOnCurve(publicKey.X, publicKey.Y) {
		return nil, nil, fmt.Errorf("device public key must be ECDSA P-256 SPKI")
	}
	canonical, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, nil, err
	}
	return canonical, publicKey, nil
}

func verifyRawP256(publicKey *ecdsa.PublicKey, payload, signature []byte) bool {
	if len(signature) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	digest := sha256.Sum256(payload)
	return ecdsa.Verify(publicKey, digest[:], r, s)
}

func cloneState(state persistentState) persistentState {
	state.Invitations = append([]invitationRecord(nil), state.Invitations...)
	for index := range state.Invitations {
		state.Invitations[index].Scopes = append([]Scope(nil), state.Invitations[index].Scopes...)
	}
	grants := state.Grants
	state.Grants = make([]Grant, len(grants))
	for index, grant := range grants {
		state.Grants[index] = cloneGrant(grant)
	}
	return state
}

func cloneGrant(grant Grant) Grant {
	grant.PublicKeySPKI = append([]byte(nil), grant.PublicKeySPKI...)
	grant.Scopes = append([]Scope(nil), grant.Scopes...)
	if grant.RevokedAt != nil {
		value := *grant.RevokedAt
		grant.RevokedAt = &value
	}
	return grant
}

func pruneInvitations(values []invitationRecord, now time.Time) []invitationRecord {
	result := make([]invitationRecord, 0, len(values))
	for _, invitation := range values {
		if now.Before(invitation.ExpiresAt) {
			result = append(result, invitation)
		}
	}
	return result
}

func grantIndex(values []Grant, id string) int {
	for index := range values {
		if values[index].ID == id {
			return index
		}
	}
	return -1
}

func humanCode(source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)
	groups := make([]string, 0, (len(raw)+4)/5)
	for len(raw) > 5 {
		groups = append(groups, raw[:5])
		raw = raw[5:]
	}
	if raw != "" {
		groups = append(groups, raw)
	}
	return strings.Join(groups, "-"), nil
}

func normalizeCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	return strings.NewReplacer("-", "", " ", "", "\t", "", "\r", "", "\n", "").Replace(value)
}

func hashCode(value string) string {
	digest := sha256.Sum256([]byte(normalizeCode(value)))
	return hex.EncodeToString(digest[:])
}

func constantHexEqual(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	if leftErr != nil || rightErr != nil || len(leftBytes) != sha256.Size || len(rightBytes) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func randomIdentifier(source io.Reader, size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomSecret(source io.Reader) (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || (index > 0 && strings.ContainsRune("._:@-", character)) {
			continue
		}
		return false
	}
	return true
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
