package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
)

func TestFederationPairsTwoInstancesAndEnforcesGuestBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hostRoot := t.TempDir()
	guestRoot := t.TempDir()
	hostCredentials := federationTestCredentials("host-instance")
	guestCredentials := federationTestCredentials("guest-instance")
	hostIdentity := federationTestIdentity(t, hostRoot, hostCredentials.InstanceID)
	guestIdentity := federationTestIdentity(t, guestRoot, guestCredentials.InstanceID)
	hostPairings := federationTestPairings(t, hostRoot, hostCredentials.InstanceID)
	guestPairings := federationTestPairings(t, guestRoot, guestCredentials.InstanceID)
	hostController := newFakeController()
	hostService, err := NewService(hostCredentials, hostController)
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(hostRoot, "federation_audit.jsonl")
	server, err := StartFederation("127.0.0.1:0", hostService, NewAuditLog(auditPath), hostIdentity, hostPairings)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Pair management runs in a separate CLI process in normal use. Use a
	// separately loaded store here to prove the live listener reloads changes.
	managementPairings := federationTestPairings(t, hostRoot, hostCredentials.InstanceID)
	invitation, err := managementPairings.CreateInvitation(server.Addr(), hostIdentity, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := invitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePairInvitation(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := AcceptPairInvitation(ctx, guestIdentity, guestPairings, decoded); err != nil {
		t.Fatal(err)
	}
	if err := AcceptPairInvitation(ctx, guestIdentity, guestPairings, decoded); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("one-time invitation reuse error=%v", err)
	}

	client, err := DialPairedPeer(ctx, guestIdentity, guestPairings, hostCredentials.InstanceID, "integration")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Join(ctx, hostController.room.ID); err != nil {
		client.Close()
		t.Fatal(err)
	}
	status, err := client.Status(ctx, hostController.room.ID)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	if status.Room.ID != hostController.room.ID {
		client.Close()
		t.Fatalf("status room=%q", status.Room.ID)
	}
	history, err := client.History(ctx, hostController.room.ID, 0, 10)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	if len(history.Messages) != 1 || history.Messages[0].Text != "hello" {
		client.Close()
		t.Fatalf("history=%+v", history)
	}
	if err := client.Ask(ctx, hostController.room.ID, "federated question"); err != nil {
		client.Close()
		t.Fatal(err)
	}
	postID, _ := NewID()
	messageID, _ := NewID()
	post := requestWithPayload(postID, "message.send", SendMessageRequest{Mode: "post", Text: "forbidden write"})
	post.RoomID = hostController.room.ID
	post.Route = &Route{MessageID: messageID, OriginInstanceID: guestIdentity.InstanceID, OriginClientID: client.identity}
	response, err := client.request(ctx, post)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "forbidden" {
		client.Close()
		t.Fatalf("remote post response=%+v", response)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	streamClient, err := DialPairedPeer(ctx, guestIdentity, guestPairings, hostCredentials.InstanceID, "event-stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := streamClient.Join(ctx, hostController.room.ID); err != nil {
		streamClient.Close()
		t.Fatal(err)
	}
	stream, err := streamClient.Subscribe(ctx, hostController.room.ID)
	if err != nil {
		streamClient.Close()
		t.Fatal(err)
	}
	hostController.events <- room.Event{Type: room.EventMessage, Message: &chat.Message{
		ID: "federated-event", Sequence: 2, Author: chat.Codex, Kind: chat.MessageText, Text: "streamed remotely",
	}}
	select {
	case event := <-stream:
		if event.Payload.Message == nil || event.Payload.Message.Text != "streamed remotely" {
			t.Fatalf("event=%+v", event)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := hostPairings.Revoke(guestIdentity.InstanceID); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("revoked event stream remained active")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not close the active peer stream")
	}
	if err := streamClient.Close(); err != nil {
		t.Fatal(err)
	}
	hostController.mu.Lock()
	if len(hostController.asks) != 1 || hostController.asks[0] != "federated question" || len(hostController.posts) != 0 {
		hostController.mu.Unlock()
		t.Fatalf("asks=%v posts=%v", hostController.asks, hostController.posts)
	}
	hostController.mu.Unlock()

	if _, err := DialPairedPeer(ctx, guestIdentity, guestPairings, hostCredentials.InstanceID, "after-revoke"); err == nil || !strings.Contains(err.Error(), "not paired or has been revoked") {
		t.Fatalf("revoked dial error=%v", err)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"action":"pair.accept"`) || !strings.Contains(string(audit), `"action":"hello"`) {
		t.Fatalf("audit=%s", audit)
	}
}

func TestFederationRejectsCertificatePinMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hostRoot := t.TempDir()
	guestRoot := t.TempDir()
	hostCredentials := federationTestCredentials("pin-host")
	guestCredentials := federationTestCredentials("pin-guest")
	hostIdentity := federationTestIdentity(t, hostRoot, hostCredentials.InstanceID)
	guestIdentity := federationTestIdentity(t, guestRoot, guestCredentials.InstanceID)
	hostPairings := federationTestPairings(t, hostRoot, hostCredentials.InstanceID)
	guestPairings := federationTestPairings(t, guestRoot, guestCredentials.InstanceID)
	service, err := NewService(hostCredentials, newFakeController())
	if err != nil {
		t.Fatal(err)
	}
	server, err := StartFederation("127.0.0.1:0", service, nil, hostIdentity, hostPairings)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	invitation, err := hostPairings.CreateInvitation(server.Addr(), hostIdentity, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	originalFingerprint := invitation.CertificateFingerprint
	invitation.CertificateFingerprint = strings.Repeat("0", 64)
	if err := AcceptPairInvitation(ctx, guestIdentity, guestPairings, invitation); err == nil || !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("pin mismatch error=%v", err)
	}
	invitation.CertificateFingerprint = originalFingerprint
	invitation.HostInstanceID = "different-host"
	if err := AcceptPairInvitation(ctx, guestIdentity, guestPairings, invitation); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("identity mismatch error=%v", err)
	}
}

func TestFederationIdentityAndPairingFilesArePrivate(t *testing.T) {
	root := t.TempDir()
	identity := federationTestIdentity(t, root, "private-instance")
	pairings := federationTestPairings(t, root, identity.InstanceID)
	invitation, err := pairings.CreateInvitation("127.0.0.1:4444", identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(FederationPairingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), invitation.Secret) {
		t.Fatal("pairing store persisted the one-time invitation secret")
	}
	for _, path := range []string{FederationIdentityPath(root), FederationPairingsPath(root)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", path, info.Mode().Perm())
		}
	}
}

func TestFederationStateRejectsSymlinksAndMalformedInvitations(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{FederationIdentityPath(root), FederationPairingsPath(root)} {
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if path == FederationIdentityPath(root) {
			if _, err := LoadOrCreateFederationIdentity(path, "symlink-instance"); err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("identity symlink error=%v", err)
			}
		} else if _, err := LoadPairingStore(path, "symlink-instance"); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("pairing symlink error=%v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	state := PairingState{Version: FederationPairVersion, Pending: []PendingPairInvitation{{
		ID: "bad-invitation", Address: "127.0.0.1:4444", SecretHash: "not-a-hash",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute),
	}}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FederationPairingsPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPairingStore(FederationPairingsPath(root), "symlink-instance"); err == nil || !strings.Contains(err.Error(), "invalid or duplicate pending") {
		t.Fatalf("malformed pending invitation error=%v", err)
	}
}

func TestFederationInvitationRejectsNonNumericOrZeroPorts(t *testing.T) {
	root := t.TempDir()
	identity := federationTestIdentity(t, root, "address-instance")
	pairings := federationTestPairings(t, root, identity.InstanceID)
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:http", "127.0.0.1:65536"} {
		if _, err := pairings.CreateInvitation(address, identity, time.Minute); err == nil {
			t.Fatalf("address %q was accepted", address)
		}
	}
}

func TestPairingStoresSerializeConcurrentManagementUpdates(t *testing.T) {
	root := t.TempDir()
	identity := federationTestIdentity(t, root, "concurrent-instance")
	const workers = 8
	const invitationsPerWorker = 5
	var group sync.WaitGroup
	errors := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			store, err := LoadPairingStore(FederationPairingsPath(root), identity.InstanceID)
			if err != nil {
				errors <- err
				return
			}
			for index := 0; index < invitationsPerWorker; index++ {
				if _, err := store.CreateInvitation("127.0.0.1:4444", identity, time.Minute); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	state := federationTestPairings(t, root, identity.InstanceID).List()
	if len(state.Pending) != workers*invitationsPerWorker {
		t.Fatalf("pending invitations=%d, want %d", len(state.Pending), workers*invitationsPerWorker)
	}
}

func federationTestCredentials(instanceID string) Credentials {
	return Credentials{InstanceID: instanceID, Entries: []Credential{{
		ID: "local-admin", Token: strings.Repeat(instanceID[:1], 64), Kind: ClientLocal,
		Scopes: []Scope{ScopeObserve, ScopeParticipate, ScopeAdminister},
	}}}
}

func federationTestIdentity(t *testing.T, root, instanceID string) *FederationIdentity {
	t.Helper()
	identity, err := LoadOrCreateFederationIdentity(FederationIdentityPath(root), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func federationTestPairings(t *testing.T, root, instanceID string) *PairingStore {
	t.Helper()
	pairings, err := LoadPairingStore(FederationPairingsPath(root), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	return pairings
}
