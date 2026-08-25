package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/testutil"
)

func TestInvitationPairingIsPrivateSingleUseAndPersistent(t *testing.T) {
	store, path := newTestStore(t)
	_, spki := testP256Key(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	invitation, err := store.CreateInvitation("room-1", "Tim's phone", []Scope{ScopeParticipate, ScopeObserve}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.Code == "" || !strings.Contains(invitation.Code, "-") {
		t.Fatalf("human code=%q", invitation.Code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), invitation.Code) || strings.Contains(string(data), normalizeCode(invitation.Code)) {
		t.Fatal("raw invitation code was persisted")
	}
	var disk persistentState
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Invitations) != 1 || !validHash(disk.Invitations[0].CodeHash) {
		t.Fatalf("persisted invitations=%+v", disk.Invitations)
	}
	grant, err := store.Pair(strings.ToLower(invitation.Code), spki)
	if err != nil {
		t.Fatal(err)
	}
	if grant.RoomID != "room-1" || grant.Name != "Tim's phone" || grant.PermissionCeiling != CeilingReadOnly ||
		len(grant.Scopes) != 2 || grant.Scopes[0] != ScopeObserve || grant.Scopes[1] != ScopeParticipate || !grant.Active() {
		t.Fatalf("grant=%+v", grant)
	}
	if _, err := store.Pair(invitation.Code, spki); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("reused code error=%v", err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	grants := restarted.List()
	if len(grants) != 1 || grants[0].ID != grant.ID || grants[0].RoomID != grant.RoomID || !grants[0].Active() {
		t.Fatalf("restarted grants=%+v", grants)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%o", info.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode=%o", parent.Mode().Perm())
	}
}

func TestScopeChangePersistsAndInvalidatesExistingSession(t *testing.T) {
	store, path := newTestStore(t)
	privateKey, grant := pairTestDevice(t, store, []Scope{ScopeObserve, ScopeParticipate})
	credentials := completeTestChallenge(t, store, privateKey, grant.ID, time.Minute, time.Hour)
	updated, err := store.SetScopes(grant.ID, []Scope{ScopeObserve})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Scopes) != 1 || updated.Scopes[0] != ScopeObserve {
		t.Fatalf("updated=%+v", updated)
	}
	select {
	case <-credentials.Session.Done:
	case <-time.After(time.Second):
		t.Fatal("scope change did not close existing session")
	}
	if _, err := store.Authenticate(credentials.Token); err == nil {
		t.Fatal("old session remained authenticated after scope change")
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persisted, ok := restarted.Grant(grant.ID)
	if !ok || len(persisted.Scopes) != 1 || persisted.Scopes[0] != ScopeObserve {
		t.Fatalf("persisted=%+v ok=%v", persisted, ok)
	}
}

func TestPairingCodeSucceedsOnlyOnceUnderConcurrency(t *testing.T) {
	store, _ := newTestStore(t)
	invitation, err := store.CreateInvitation("room", "phone", []Scope{ScopeObserve}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	var group sync.WaitGroup
	group.Add(contenders)
	results := make(chan error, contenders)
	for range contenders {
		go func() {
			defer group.Done()
			_, spki := testP256Key(t)
			_, err := store.Pair(invitation.Code, spki)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || len(store.List()) != 1 {
		t.Fatalf("successful pairings=%d grants=%d", successes, len(store.List()))
	}
}

func TestChallengeVerifiesWebCryptoRawSignatureAndMintsExpiringSession(t *testing.T) {
	store, _ := newTestStore(t)
	privateKey, grant := pairTestDevice(t, store, []Scope{ScopeObserve, ScopeParticipate})
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	challenge, err := store.NewChallenge(grant.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signature := signRawP256(t, privateKey, []byte(challenge.Payload))
	credentials, err := store.CompleteChallenge(grant.ID, challenge.ID, signature, 2*time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Token == "" || credentials.CSRFToken == "" || credentials.Session.DeviceID != grant.ID ||
		credentials.Session.RoomID != grant.RoomID || credentials.Session.PermissionCeiling != CeilingReadOnly {
		t.Fatalf("credentials=%+v", credentials)
	}
	if !store.VerifyCSRF(credentials.Session.ID, credentials.CSRFToken) || store.VerifyCSRF(credentials.Session.ID, "wrong") {
		t.Fatal("CSRF verification did not bind the session secret")
	}
	authenticated, err := store.Authenticate(credentials.Token)
	if err != nil || authenticated.ID != credentials.Session.ID {
		t.Fatalf("authenticate=%+v err=%v", authenticated, err)
	}
	if _, err := store.Authenticate("wrong-token"); err == nil {
		t.Fatal("wrong token authenticated")
	}

	// Refresh just before the idle deadline, then prove the independent absolute
	// deadline still terminates the session.
	now = now.Add(90 * time.Second)
	authenticated, err = store.Authenticate(credentials.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated.IdleExpiresAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("refreshed idle expiry=%s", authenticated.IdleExpiresAt)
	}
	now = credentials.Session.AbsoluteExpiresAt
	if _, err := store.Authenticate(credentials.Token); err == nil {
		t.Fatal("absolute-expired session authenticated")
	}
	select {
	case <-credentials.Session.Done:
	default:
		t.Fatal("expired session did not close Done")
	}

	// A new session independently expires on idle time even while its absolute
	// lifetime still has ample time remaining.
	idleChallenge, err := store.NewChallenge(grant.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	idleCredentials, err := store.CompleteChallenge(grant.ID, idleChallenge.ID, signRawP256(t, privateKey, []byte(idleChallenge.Payload)), time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := store.Authenticate(idleCredentials.Token); err == nil {
		t.Fatal("idle-expired session authenticated")
	}
	select {
	case <-idleCredentials.Session.Done:
	default:
		t.Fatal("idle-expired session did not close Done")
	}
}

func TestChallengeIsSingleAttemptAndExpires(t *testing.T) {
	store, _ := newTestStore(t)
	privateKey, grant := pairTestDevice(t, store, []Scope{ScopeObserve})
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	challenge, err := store.NewChallenge(grant.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteChallenge(grant.ID, challenge.ID, make([]byte, 64), time.Minute, time.Hour); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("bad signature error=%v", err)
	}
	valid := signRawP256(t, privateKey, []byte(challenge.Payload))
	if _, err := store.CompleteChallenge(grant.ID, challenge.ID, valid, time.Minute, time.Hour); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("reused challenge error=%v", err)
	}

	expiring, err := store.NewChallenge(grant.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := store.CompleteChallenge(grant.ID, expiring.ID, signRawP256(t, privateKey, []byte(expiring.Payload)), time.Minute, time.Hour); err == nil {
		t.Fatal("expired challenge completed")
	}
}

func TestRevocationPersistsAndInvalidatesLiveSessions(t *testing.T) {
	store, path := newTestStore(t)
	privateKey, grant := pairTestDevice(t, store, []Scope{ScopeObserve, ScopeParticipate})
	challenge, err := store.NewChallenge(grant.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.CompleteChallenge(grant.ID, challenge.ID, signRawP256(t, privateKey, []byte(challenge.Payload)), time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(grant.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-credentials.Session.Done:
	case <-time.After(time.Second):
		t.Fatal("revocation did not close session Done")
	}
	if _, err := store.Authenticate(credentials.Token); err == nil {
		t.Fatal("revoked session authenticated")
	}
	if _, err := store.NewChallenge(grant.ID, time.Minute); err == nil {
		t.Fatal("revoked device received a challenge")
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	grants := restarted.List()
	if len(grants) != 1 || grants[0].RevokedAt == nil || grants[0].ID != grant.ID {
		t.Fatalf("restarted grants=%+v", grants)
	}
}

func TestStoreRejectsInvalidScopesKeysSymlinksAndModes(t *testing.T) {
	store, _ := newTestStore(t)
	for _, scopes := range [][]Scope{{}, {ScopeParticipate}, {ScopeObserve, ScopeObserve}, {ScopeObserve, Scope("admin")}} {
		if _, err := store.CreateInvitation("room", "phone", scopes, time.Minute); err == nil {
			t.Fatalf("invalid scopes accepted: %v", scopes)
		}
	}
	invitation, err := store.CreateInvitation("room", "phone", []Scope{ScopeObserve}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongCurve, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pair(invitation.Code, wrongCurve); err == nil || !strings.Contains(err.Error(), "P-256") {
		t.Fatalf("wrong curve error=%v", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	directory := testutil.CanonicalTempDir(t)
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "devices.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("state symlink error=%v", err)
	}
	privateDirectory := filepath.Join(testutil.CanonicalTempDir(t), "private")
	if err := os.Mkdir(privateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	insecureFile := filepath.Join(privateDirectory, "devices.json")
	if err := os.WriteFile(insecureFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(insecureFile); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("insecure state mode error=%v", err)
	}

	insecureDirectory := filepath.Join(testutil.CanonicalTempDir(t), "insecure")
	if err := os.Mkdir(insecureDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(insecureDirectory, "devices.json")); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("insecure parent error=%v", err)
	}

	realParent := filepath.Join(testutil.CanonicalTempDir(t), "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(testutil.CanonicalTempDir(t), "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(filepath.Join(linkedParent, "devices.json")); err == nil || !strings.Contains(err.Error(), "must not traverse symbolic links") {
		t.Fatalf("symlinked parent error=%v", err)
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("remote device state requires POSIX private-file modes; native Windows support is preview")
	}
	directory := filepath.Join(testutil.CanonicalTempDir(t), "state")
	path := filepath.Join(directory, "remote_devices.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, path
}

func pairTestDevice(t *testing.T, store *Store, scopes []Scope) (*ecdsa.PrivateKey, Grant) {
	t.Helper()
	privateKey, spki := testP256Key(t)
	invitation, err := store.CreateInvitation("room", "phone", scopes, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Pair(invitation.Code, spki)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, grant
}

func completeTestChallenge(t *testing.T, store *Store, privateKey *ecdsa.PrivateKey, deviceID string, idleTTL, absoluteTTL time.Duration) SessionCredentials {
	t.Helper()
	challenge, err := store.NewChallenge(deviceID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := store.CompleteChallenge(deviceID, challenge.ID, signRawP256(t, privateKey, []byte(challenge.Payload)), idleTTL, absoluteTTL)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func testP256Key(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, spki
}

func signRawP256(t *testing.T, privateKey *ecdsa.PrivateKey, payload []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(payload)
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return append(paddedBigInt(r, 32), paddedBigInt(s, 32)...)
}

func paddedBigInt(value *big.Int, size int) []byte {
	result := make([]byte, size)
	value.FillBytes(result)
	return result
}
