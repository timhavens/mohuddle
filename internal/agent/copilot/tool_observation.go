package copilot

import (
	sdk "github.com/github/copilot-sdk/go"
	"github.com/timhavens/mohuddle/internal/agent"
)

func copilotToolStart(tracker *agent.ToolTracker, data *sdk.ToolExecutionStartData, workspace string) *agent.ToolObservation {
	name := agent.ToolFingerprint(data.ToolName, data.MCPServerName, data.MCPToolName)
	if data.ToolName == "" {
		name = ""
	}
	return tracker.Start(data.ToolCallID, "copilot", name, workspace, data.Arguments)
}

func copilotToolComplete(tracker *agent.ToolTracker, data *sdk.ToolExecutionCompleteData) *agent.ToolObservation {
	outcome := agent.ToolFailed
	if data.Success {
		outcome = agent.ToolSucceeded
	}
	// SDK errors do not establish JSON-RPC retryability. Keep them advisory.
	return tracker.Finish(data.ToolCallID, outcome, []any{data.Result, data.Error})
}
