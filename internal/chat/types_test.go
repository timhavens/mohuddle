package chat

import (
	"testing"
	"time"
)

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
