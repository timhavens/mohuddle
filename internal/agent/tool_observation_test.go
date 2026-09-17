package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolFingerprintsPreserveOperationDetails(t *testing.T) {
	a := ToolFingerprint("tool", json.RawMessage(`{"b":2,"a":9007199254740992}`))
	b := ToolFingerprint("tool", json.RawMessage(`{ "a":9007199254740992, "b":2 }`))
	if a == "" || a != b {
		t.Fatal("equivalent JSON lost identity")
	}
	if a == ToolFingerprint("tool", json.RawMessage(`{"b":2,"a":9007199254740993}`)) {
		t.Fatal("large integer identities collapsed")
	}
	seen := map[string]bool{}
	for _, command := range []any{"printf A", "printf a", "printf 'a  b'", "printf 'a b'", []string{"echo", "a b"}, []string{"echo a", "b"}} {
		key := ToolFingerprint("shell", "/workspace", command)
		if key == "" || seen[key] {
			t.Fatalf("command identities collided: %v", command)
		}
		seen[key] = true
	}
	if ToolFingerprint("shell", "/a", "pwd") == ToolFingerprint("shell", "/b", "pwd") {
		t.Fatal("cwd lost")
	}
	if ToolFingerprint(json.RawMessage(`{broken`)) != "" {
		t.Fatal("malformed input got an identity")
	}
}

func TestToolEvidenceStaysPrivate(t *testing.T) {
	key := ToolFingerprint("private-token", "private-command")
	e := Event{Type: EventTool, Text: "safe summary", ToolAction: &key, ToolObservation: &ToolObservation{InvocationID: "private-call", Operation: key, Result: "private-result"}}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key, "private-call", "private-result", "ToolObservation", "ToolAction"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private evidence exported: %s", encoded)
		}
	}
}

func TestToolTrackerRetainsExactInputsAndUnknowns(t *testing.T) {
	var tracker ToolTracker
	a := tracker.Start("1", "claude", "MCP", "/w", json.RawMessage(`{"id":9007199254740992}`))
	b := tracker.Start("2", "claude", "MCP", "/w", json.RawMessage(`{"id":9007199254740993}`))
	if !a.Complete || !b.Complete || a.Operation == b.Operation {
		t.Fatal("raw input precision lost")
	}
	if tracker.Start("3", "copilot", "MCP", "/w", map[string]any{"id": float64(9007199254740992)}).Complete {
		t.Fatal("rounded SDK number claimed complete")
	}
	if tracker.Start("4", "claude", "MCP", "/w", json.RawMessage(nil)).Complete {
		t.Fatal("missing args claimed complete")
	}
	end := tracker.Finish("1", ToolFailed, json.RawMessage(`{"error":"failed"}`))
	if end.Operation != a.Operation || end.Result == "" || end.NonTransient {
		t.Fatal("correlation or retryability wrong")
	}
	if tracker.Finish("orphan", ToolFailed, "x").Complete {
		t.Fatal("orphan claimed complete")
	}
}

func TestPermanentProtocolErrorRequiresStructuredEvidence(t *testing.T) {
	for _, raw := range []string{`{"code":-32600}`, `{"code":-32601}`, `{"code":-32602}`} {
		if !PermanentProtocolError(json.RawMessage(raw)) {
			t.Fatalf("missed explicit failure %s", raw)
		}
	}
	for _, raw := range []string{`{"code":-32000}`, `{"message":"invalid arguments"}`, `{"code":"-32602"}`, `{"code":429}`, `null`, `broken`} {
		if PermanentProtocolError(json.RawMessage(raw)) {
			t.Fatalf("inferred permanent failure from %s", raw)
		}
	}
}
