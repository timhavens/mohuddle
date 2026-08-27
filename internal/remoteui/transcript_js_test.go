package remoteui

import (
	"os/exec"
	"testing"
)

func TestPhoneTranscriptJavaScriptRegressions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	command := exec.Command(node, "--test", "transcript_test.mjs")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("phone transcript JavaScript tests failed: %v\n%s", err, output)
	}
}
