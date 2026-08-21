package agent

import (
	"strings"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestParseControl(t *testing.T) {
	value := "Work is complete.\n<!-- mohuddle:{\"done\":true} -->"
	public, done, request := ParseControl(value)
	if public != "Work is complete." || !done || request != nil {
		t.Fatalf("unexpected parse: public=%q done=%v request=%+v", public, done, request)
	}
}

func TestParseControlAccessRequest(t *testing.T) {
	value := "I need more context.\n<!-- mohuddle-access:{\"path\":\"../booking\",\"mode\":\"read_write\",\"reason\":\"inspect tests\"} -->\n<!-- mohuddle:{\"done\":false} -->"
	public, done, request := ParseControl(value)
	if public != "I need more context." || done || request == nil {
		t.Fatalf("unexpected parse: public=%q done=%v request=%+v", public, done, request)
	}
	if request.Path != "../booking" || request.Mode != chat.AccessReadWrite || request.Reason != "inspect tests" {
		t.Fatalf("unexpected access request: %+v", request)
	}
}

func TestParseControlLeavesMalformedMarkerVisible(t *testing.T) {
	value := "hello\n<!-- mohuddle:{not-json} -->"
	public, done, _ := ParseControl(value)
	if public != value || done {
		t.Fatalf("malformed marker was hidden: %q", public)
	}
}

func TestParseResponseReportsMaterialDisagreement(t *testing.T) {
	value := "The proposed migration can lose data.\n<!-- mohuddle:{\"done\":false,\"position\":\"disagree\",\"reason\":\"unsafe migration order\"} -->"
	public, state, request := ParseResponse(value)
	if public != "The proposed migration can lose data." || state.Done || state.Position != "disagree" || state.Reason != "unsafe migration order" || request != nil {
		t.Fatalf("public=%q state=%+v request=%+v", public, state, request)
	}
}

func TestFullAccessPromptRemovesDirectoryRequestInstruction(t *testing.T) {
	prompt := RoomProtocolPromptFor(chat.AgentSettings{Permissions: chat.PermissionFull})
	if strings.Contains(prompt, "If you need a directory outside") || !strings.Contains(prompt, "full-machine filesystem and network access") {
		t.Fatalf("unexpected full-access prompt: %s", prompt)
	}
}
