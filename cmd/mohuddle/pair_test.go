package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
)

func TestPairInviteAndListCommandsKeepSecretsOutOfListings(t *testing.T) {
	stateDir := t.TempDir()
	var invitationOutput bytes.Buffer
	if err := runPairCommandIO([]string{
		"invite", "--state-dir", stateDir, "--address", "127.0.0.1:4444", "--ttl", "5m",
	}, strings.NewReader(""), &invitationOutput); err != nil {
		t.Fatal(err)
	}
	invitation, err := api.DecodePairInvitation(strings.TrimSpace(invitationOutput.String()))
	if err != nil {
		t.Fatal(err)
	}
	if invitation.ExpiresAt.Before(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("invitation expiry=%s", invitation.ExpiresAt)
	}
	var listOutput bytes.Buffer
	if err := runPairCommandIO([]string{"list", "--state-dir", stateDir}, strings.NewReader(""), &listOutput); err != nil {
		t.Fatal(err)
	}
	listing := listOutput.String()
	if !strings.Contains(listing, invitation.HostInstanceID) || strings.Contains(listing, invitation.Secret) || strings.Contains(listing, "private_key") || strings.Contains(listing, "token") {
		t.Fatalf("pair listing=%s", listing)
	}
}

func TestPairAcceptReadsInvitationFromStdin(t *testing.T) {
	value := "mohuddle-pair-v1:payload"
	got, err := pairingCode(nil, strings.NewReader(value+"\nignored"))
	if err != nil {
		t.Fatal(err)
	}
	if got != value {
		t.Fatalf("pairing code=%q", got)
	}
}

func TestPairCommandRejectsUnknownOrIncompleteOperations(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"unknown"},
		{"invite", "--address", ""},
		{"revoke"},
		{"check", "--peer", "peer-only"},
	} {
		if err := runPairCommandIO(arguments, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Fatalf("arguments %v were accepted", arguments)
		}
	}
}
