package api

import (
	"path/filepath"
	"testing"
)

func TestAuditRecentReturnsBoundedCredentialFreeRecords(t *testing.T) {
	log := NewAuditLog(filepath.Join(t.TempDir(), "api_audit.jsonl"))
	for _, value := range []AuditRecord{
		{Action: "first", DeviceID: "phone", SessionID: "session-1", Scopes: []Scope{ScopeObserve}, Allowed: true},
		{Action: "second", DeviceID: "phone", RoomID: "room", Permission: "read-only", Allowed: false, Error: "denied"},
		{Action: "third", Allowed: true},
	} {
		if err := log.Append(value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := log.Recent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Action != "second" || values[1].Action != "third" {
		t.Fatalf("recent=%+v", values)
	}
}
