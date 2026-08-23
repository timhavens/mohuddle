package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/store"
)

func runPairCommand(args []string) error {
	return runPairCommandIO(args, os.Stdin, os.Stdout)
}

func runPairCommandIO(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("pair command is required: invite, accept, list, revoke, or check")
	}
	switch args[0] {
	case "invite":
		flags := flag.NewFlagSet("pair invite", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		stateDir := flags.String("state-dir", "", "state directory")
		address := flags.String("address", "", "advertised federation host:port")
		ttl := flags.Duration("ttl", 15*time.Minute, "invitation lifetime")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || strings.TrimSpace(*address) == "" {
			return fmt.Errorf("usage: mohuddle pair invite --address HOST:PORT [--ttl 15m] [--state-dir DIR]")
		}
		_, identity, pairings, err := openFederationState(*stateDir)
		if err != nil {
			return err
		}
		invitation, err := pairings.CreateInvitation(*address, identity, *ttl)
		if err != nil {
			return err
		}
		encoded, err := invitation.Encode()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, encoded)
		return err

	case "accept":
		flags := flag.NewFlagSet("pair accept", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		stateDir := flags.String("state-dir", "", "state directory")
		timeout := flags.Duration("timeout", 15*time.Second, "connection timeout")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		code, err := pairingCode(flags.Args(), input)
		if err != nil {
			return err
		}
		invitation, err := api.DecodePairInvitation(code)
		if err != nil {
			return err
		}
		credentials, identity, pairings, err := openFederationState(*stateDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		if err := api.AcceptPairInvitation(ctx, identity, pairings, invitation); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "paired %s with %s\n", credentials.InstanceID, invitation.HostInstanceID)
		return err

	case "list":
		flags := flag.NewFlagSet("pair list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		stateDir := flags.String("state-dir", "", "state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("usage: mohuddle pair list [--state-dir DIR]")
		}
		credentials, _, pairings, err := openFederationState(*stateDir)
		if err != nil {
			return err
		}
		return writePairingSummary(output, credentials.InstanceID, pairings.List())

	case "revoke":
		flags := flag.NewFlagSet("pair revoke", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		stateDir := flags.String("state-dir", "", "state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: mohuddle pair revoke [--state-dir DIR] INSTANCE_ID")
		}
		_, _, pairings, err := openFederationState(*stateDir)
		if err != nil {
			return err
		}
		if err := pairings.Revoke(flags.Arg(0)); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "revoked %s\n", flags.Arg(0))
		return err

	case "check":
		flags := flag.NewFlagSet("pair check", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		stateDir := flags.String("state-dir", "", "state directory")
		peer := flags.String("peer", "", "paired remote instance identity")
		roomID := flags.String("room", "", "remote room identity")
		timeout := flags.Duration("timeout", 15*time.Second, "connection timeout")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *peer == "" || *roomID == "" {
			return fmt.Errorf("usage: mohuddle pair check --peer INSTANCE_ID --room ROOM_ID [--state-dir DIR]")
		}
		_, identity, pairings, err := openFederationState(*stateDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		client, err := api.DialPairedPeer(ctx, identity, pairings, *peer, "pair-check")
		if err != nil {
			return err
		}
		defer client.Close()
		if err := client.Join(ctx, *roomID); err != nil {
			return err
		}
		status, err := client.Status(ctx, *roomID)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)

	default:
		return fmt.Errorf("unknown pair command %q: use invite, accept, list, revoke, or check", args[0])
	}
}

func openFederationState(stateDir string) (*api.Credentials, *api.FederationIdentity, *api.PairingStore, error) {
	roomStore, err := store.New(stateDir)
	if err != nil {
		return nil, nil, nil, err
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(roomStore.Root()))
	if err != nil {
		return nil, nil, nil, err
	}
	identity, err := api.LoadOrCreateFederationIdentity(api.FederationIdentityPath(roomStore.Root()), credentials.InstanceID)
	if err != nil {
		return nil, nil, nil, err
	}
	pairings, err := api.LoadPairingStore(api.FederationPairingsPath(roomStore.Root()), credentials.InstanceID)
	if err != nil {
		return nil, nil, nil, err
	}
	return credentials, identity, pairings, nil
}

func pairingCode(arguments []string, input io.Reader) (string, error) {
	if len(arguments) > 1 {
		return "", fmt.Errorf("pair accept takes at most one invitation argument")
	}
	if len(arguments) == 1 {
		return strings.TrimSpace(arguments[0]), nil
	}
	value, err := bufio.NewReader(io.LimitReader(input, api.MaxFrameBytes)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("pairing invitation is required as an argument or on stdin")
	}
	return value, nil
}

type peerSummary struct {
	InstanceID             string    `json:"instance_id"`
	Address                string    `json:"address,omitempty"`
	CertificateFingerprint string    `json:"certificate_fingerprint"`
	CreatedAt              time.Time `json:"created_at"`
}

func writePairingSummary(output io.Writer, instanceID string, state api.PairingState) error {
	result := struct {
		InstanceID string        `json:"instance_id"`
		Inbound    []peerSummary `json:"inbound"`
		Outbound   []peerSummary `json:"outbound"`
	}{InstanceID: instanceID, Inbound: summarizePeers(state.Inbound), Outbound: summarizePeers(state.Outbound)}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func summarizePeers(values []api.PairedPeer) []peerSummary {
	result := make([]peerSummary, 0, len(values))
	for _, peer := range values {
		result = append(result, peerSummary{
			InstanceID: peer.InstanceID, Address: peer.Address,
			CertificateFingerprint: peer.CertificateFingerprint, CreatedAt: peer.CreatedAt,
		})
	}
	return result
}
