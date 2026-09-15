package chatgpt

import (
	"os/exec"
	"testing"
)

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
