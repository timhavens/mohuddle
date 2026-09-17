package copilot

import (
	sdk "github.com/github/copilot-sdk/go"
	"github.com/timhavens/mohuddle/internal/agent"
	"testing"
)

func TestCopilotCorrelatesCallsWithoutDisplayIdentity(t *testing.T) {
	var tracker agent.ToolTracker
	a := copilotToolStart(&tracker, &sdk.ToolExecutionStartData{ToolCallID: "a", ToolName: "bash", Arguments: map[string]any{"command": "echo A"}}, "/a")
	b := copilotToolStart(&tracker, &sdk.ToolExecutionStartData{ToolCallID: "b", ToolName: "bash", Arguments: map[string]any{"command": "echo B"}}, "/a")
	if !a.Complete || !b.Complete || a.Operation == b.Operation {
		t.Fatal("lost distinct operations")
	}
	endB := copilotToolComplete(&tracker, &sdk.ToolExecutionCompleteData{ToolCallID: "b", Success: false})
	endA := copilotToolComplete(&tracker, &sdk.ToolExecutionCompleteData{ToolCallID: "a", Success: true})
	if endB.Operation != b.Operation || endA.Operation != a.Operation || endA.Outcome != agent.ToolSucceeded || endB.Outcome != agent.ToolFailed || endB.NonTransient {
		t.Fatal("lost correlation or inferred permanent error")
	}
	if copilotToolComplete(&tracker, &sdk.ToolExecutionCompleteData{ToolCallID: "missing"}).Complete {
		t.Fatal("orphan was trusted")
	}
}
