package chat

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestAuxiliaryParticipantIdentity(t *testing.T) {
	tests := []struct {
		value     Participant
		provider  Participant
		index     int
		valid     bool
		auxiliary bool
	}{
		{Codex, Codex, 0, true, false},
		{Participant("codex-1"), Codex, 1, true, true},
		{Participant("claude-23"), Claude, 23, true, true},
		{Participant("agy-3"), Agy, 3, true, true},
		{Participant("copilot-9"), Copilot, 9, true, true},
		{"codex-0", "", 0, false, false},
		{"codex-01", "", 0, false, false},
		{"codex--1", "", 0, false, false},
		{"codex-1-2", "", 0, false, false},
		{"codex-one", "", 0, false, false},
		{"unknown-1", "", 0, false, false},
		{User, "", 0, false, false},
		{"", "", 0, false, false},
	}
	for _, test := range tests {
		t.Run(string(test.value), func(t *testing.T) {
			if got := test.value.Provider(); got != test.provider {
				t.Fatalf("Provider()=%q want=%q", got, test.provider)
			}
			if got := test.value.ValidAgent(); got != test.valid {
				t.Fatalf("ValidAgent()=%t want=%t", got, test.valid)
			}
			if got := test.value.IsAuxiliary(); got != test.auxiliary {
				t.Fatalf("IsAuxiliary()=%t want=%t", got, test.auxiliary)
			}
			if got := test.value.AuxiliaryIndex(); got != test.index {
				t.Fatalf("AuxiliaryIndex()=%d want=%d", got, test.index)
			}
		})
	}

	for _, provider := range Agents() {
		participant, ok := AuxiliaryParticipant(provider, 2)
		if !ok || participant != Participant(fmt.Sprintf("%s-2", provider)) {
			t.Fatalf("AuxiliaryParticipant(%q, 2)=(%q, %t)", provider, participant, ok)
		}
	}
	for _, invalid := range []struct {
		provider Participant
		index    int
	}{{Codex, 0}, {"codex-1", 2}, {User, 1}, {"unknown", 1}} {
		if participant, ok := AuxiliaryParticipant(invalid.provider, invalid.index); ok || participant != "" {
			t.Fatalf("AuxiliaryParticipant(%q, %d)=(%q, %t), want invalid", invalid.provider, invalid.index, participant, ok)
		}
	}
}

func TestAuxiliaryParticipantsAreOrderedAndReadOnly(t *testing.T) {
	input := []Participant{"copilot-2", Claude, "codex-2", Copilot, "agy-1", Codex, "claude-1", Agy, "codex-1"}
	want := []Participant{Codex, "codex-1", "codex-2", Claude, "claude-1", Agy, "agy-1", Copilot, "copilot-2"}
	if got := OrderedParticipants(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("OrderedParticipants()=%v want=%v", got, want)
	}
	if !reflect.DeepEqual(input, []Participant{"copilot-2", Claude, "codex-2", Copilot, "agy-1", Codex, "claude-1", Agy, "codex-1"}) {
		t.Fatalf("OrderedParticipants mutated input: %v", input)
	}
	for _, participant := range []Participant{"codex-1", "claude-1", "agy-1", "copilot-1"} {
		if got := participant.DefaultPermissions(); got != PermissionReadOnly {
			t.Fatalf("%s default permissions=%q want=%q", participant, got, PermissionReadOnly)
		}
	}
}

func TestCorePolicyRejectsAuxiliaryParticipants(t *testing.T) {
	for name, policy := range map[string]CorePolicy{
		"preferred": {Preferred: []Participant{"codex-1"}, Failover: CoreFailoverAuto, Restore: CoreRestoreAuto},
		"fallback":  {Preferred: []Participant{Codex}, Fallbacks: []Participant{"claude-1"}, Failover: CoreFailoverAuto, Restore: CoreRestoreAuto},
	} {
		t.Run(name, func(t *testing.T) {
			if err := policy.Validate(); err == nil {
				t.Fatal("Validate() accepted auxiliary core participant")
			}
		})
	}
	normalized := (CorePolicy{
		Preferred: []Participant{"codex-1", Claude},
		Fallbacks: []Participant{"agy-1", Copilot},
		Failover:  CoreFailoverAuto,
		Restore:   CoreRestoreAuto,
	}).WithDefaults()
	if !reflect.DeepEqual(normalized.Preferred, []Participant{Claude}) || !reflect.DeepEqual(normalized.Fallbacks, []Participant{Copilot}) {
		t.Fatalf("WithDefaults() retained auxiliary core participants: %+v", normalized)
	}
}

func TestRoomPresentAgentsIncludesDynamicAuxiliaries(t *testing.T) {
	room := Room{Members: map[Participant]bool{
		Claude: true, "claude-2": true, "claude-1": false,
		Codex: true, "codex-1": true, "agy-3": true,
		User: true, System: true, "unknown-1": true,
	}}
	want := []Participant{Codex, "codex-1", Claude, "claude-2", "agy-3"}
	if got := room.PresentAgents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PresentAgents()=%v want=%v", got, want)
	}
	if got := (Room{}).PresentAgents(); !reflect.DeepEqual(got, DefaultAgents()) {
		t.Fatalf("nil-member PresentAgents()=%v want=%v", got, DefaultAgents())
	}
	if got := (Room{Members: map[Participant]bool{}}).PresentAgents(); len(got) != 0 {
		t.Fatalf("empty-member PresentAgents()=%v want empty", got)
	}
}

func TestCorrectionStatisticsReplaysAuditableEvents(t *testing.T) {
	now := time.Now().UTC()
	messages := []Message{
		message(1, Claude, "claim", now),
		correctionMessage(2, Codex, "correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 2, CorrectedSequence: 1, Proposer: Codex, Target: Claude}),
		correctionMessage(3, Claude, "accepted", now, CorrectionEvent{Type: CorrectionAccepted, CorrectionSequence: 2}),
		message(4, Agy, "claim", now),
		correctionMessage(5, Codex, "correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 5, CorrectedSequence: 4, Proposer: Codex, Target: Agy}),
		correctionMessage(6, Agy, "disputed", now, CorrectionEvent{Type: CorrectionDisputed, CorrectionSequence: 5}),
		message(7, Codex, "claim", now),
		correctionMessage(8, Claude, "correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 8, CorrectedSequence: 7, Proposer: Claude, Target: Codex}),
		correctionMessage(9, Claude, "retracted", now, CorrectionEvent{Type: CorrectionRetracted, CorrectionSequence: 8}),
		message(10, Agy, "claim", now),
		correctionMessage(11, Copilot, "correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 11, CorrectedSequence: 10, Proposer: Copilot, Target: Agy}),
	}
	total, agents := CorrectionStatistics(messages)
	if total.Offered != 4 || total.Accepted != 1 || total.Retracted != 1 || total.Pending != 2 {
		t.Fatalf("room counts=%+v", total)
	}
	if got := agents[Codex]; got.Offered != 2 || got.Accepted != 1 || got.Pending != 1 || got.AcceptedReceived != 0 {
		t.Fatalf("codex counts=%+v", got)
	}
	if got := agents[Claude]; got.Offered != 1 || got.Retracted != 1 || got.AcceptedReceived != 1 {
		t.Fatalf("claude counts=%+v", got)
	}
	ledger := CorrectionLedger(messages)
	if len(ledger) != 4 || ledger[1].Status != CorrectionDisputedStatus || ledger[1].StatusSequence != 6 {
		t.Fatalf("ledger=%+v", ledger)
	}
}

func TestCorrectionLedgerRejectsCorruptDuplicateAndUnauthorizedEvents(t *testing.T) {
	now := time.Now().UTC()
	messages := []Message{
		message(1, Claude, "claim", now),
		correctionMessage(2, Codex, "valid", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 2, CorrectedSequence: 1, Proposer: Codex, Target: Claude}),
		correctionMessage(3, Agy, "unauthorized accept", now, CorrectionEvent{Type: CorrectionAccepted, CorrectionSequence: 2}),
		correctionMessage(4, Claude, "accepted", now, CorrectionEvent{Type: CorrectionAccepted, CorrectionSequence: 2}),
		correctionMessage(5, Codex, "too late", now, CorrectionEvent{Type: CorrectionRetracted, CorrectionSequence: 2}),
		correctionMessage(6, Codex, "missing source", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 6, CorrectedSequence: 99, Proposer: Codex, Target: Claude}),
		correctionMessage(7, Codex, "wrong proposer", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 7, CorrectedSequence: 1, Proposer: User, Target: Claude}),
		correctionMessage(8, Codex, "duplicate sequence one", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 8, CorrectedSequence: 1, Proposer: Codex, Target: Claude}),
		correctionMessage(8, Codex, "duplicate sequence two", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 8, CorrectedSequence: 1, Proposer: Codex, Target: Claude}),
	}
	ledger := CorrectionLedger(messages)
	if len(ledger) != 1 || ledger[0].Status != CorrectionAcceptedStatus || ledger[0].StatusSequence != 4 {
		t.Fatalf("ledger=%+v", ledger)
	}
	total, _ := CorrectionStatistics(messages)
	if total.Offered != 1 || total.Accepted != 1 || total.Retracted != 0 || total.Pending != 0 {
		t.Fatalf("counts=%+v", total)
	}
}

func TestCorrectionLedgerRejectsImpossiblePerMessageEventShapes(t *testing.T) {
	now := time.Now().UTC()
	messages := []Message{
		message(1, Claude, "first claim", now),
		correctionMessage(2, Codex, "first correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 2, CorrectedSequence: 1, Proposer: Codex, Target: Claude}),
		message(3, Claude, "second claim", now),
		correctionMessage(4, Codex, "second correction", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 4, CorrectedSequence: 3, Proposer: Codex, Target: Claude}),
		correctionMessage(5, Claude, "corrupt multiple acceptance", now,
			CorrectionEvent{Type: CorrectionAccepted, CorrectionSequence: 2},
			CorrectionEvent{Type: CorrectionAccepted, CorrectionSequence: 4},
		),
		message(6, Agy, "", now),
		correctionMessage(7, Copilot, "correction against empty text", now, CorrectionEvent{Type: CorrectionOffered, CorrectionSequence: 7, CorrectedSequence: 6, Proposer: Copilot, Target: Agy}),
	}

	ledger := CorrectionLedger(messages)
	if len(ledger) != 2 {
		t.Fatalf("ledger=%+v", ledger)
	}
	for _, correction := range ledger {
		if correction.Status != CorrectionPendingStatus {
			t.Fatalf("correction %d status=%s, want pending", correction.CorrectionSequence, correction.Status)
		}
	}
	total, _ := CorrectionStatistics(messages)
	if total.Offered != 2 || total.Accepted != 0 || total.Pending != 2 {
		t.Fatalf("counts=%+v", total)
	}
}

func message(sequence uint64, author Participant, text string, createdAt time.Time) Message {
	return Message{Sequence: sequence, Author: author, Kind: MessageText, Text: text, CreatedAt: createdAt}
}

func correctionMessage(sequence uint64, author Participant, text string, createdAt time.Time, events ...CorrectionEvent) Message {
	value := message(sequence, author, text, createdAt)
	value.CorrectionEvents = events
	return value
}
