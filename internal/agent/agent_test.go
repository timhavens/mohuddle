package agent

import (
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
