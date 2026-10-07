package chatgpt

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPanelResourceCacheIdentityTracksRenderedContents(t *testing.T) {
	document := panelDocument()
	if PanelURI != panelResourceURI(document) {
		t.Fatal("resource descriptor does not identify the served contents")
	}
	if PanelURI == panelResourceURI(document+"\n<!-- new panel revision -->") {
		t.Fatal("updated HTML would reuse cached resource contents")
	}
	if PanelURI != panelResourceURI(strings.ReplaceAll(document, "\n", "\r\n")) {
		t.Fatal("Windows checkout would advertise a different component revision")
	}
}

func TestLivePanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; CI also runs the panel tests explicitly")
	}
	output, err := exec.CommandContext(t.Context(), node, "--test", "panel_test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("live panel tests: %v\n%s", err, output)
	}
}
