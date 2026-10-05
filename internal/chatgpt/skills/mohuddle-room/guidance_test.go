package roomguidance

import (
	"strings"
	"testing"
)

func TestEmbeddedGuidanceSectionsAreComplete(t *testing.T) {
	// Native Windows CI also exercises Markdown checked out with CRLF.
	for _, tc := range []struct{ name, text, required string }{
		{"reminder", Brief, "shared resource needed by many rooms"},
		{"participant", Participant, "Other rooms need this shared workspace"},
		{"coordinator", section("Coordinator procedure"), "On joining or reconnecting"},
	} {
		if !strings.Contains(tc.text, tc.required) {
			t.Fatalf("%s omitted its required instructions", tc.name)
		}
	}
	if strings.Contains(Skill+Coordination, "\r\n") {
		t.Fatal("embedded guidance must have canonical line endings for version hashing")
	}
}
