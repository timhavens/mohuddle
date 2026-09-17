package codex

import (
	"encoding/json"

	"github.com/timhavens/mohuddle/internal/agent"
)

// ObservationFromItem keeps the app-server payload separate from its public
// summary. Exported for deterministic adapter-to-orchestrator replay tests.
func ObservationFromItem(raw json.RawMessage, workspace string, phase agent.ToolPhase) *agent.ToolObservation {
	var params struct {
		Item map[string]json.RawMessage `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return &agent.ToolObservation{Phase: phase}
	}
	item := params.Item
	text := func(key string) string { var v string; _ = json.Unmarshal(item[key], &v); return v }
	kind := text("type")
	if kind != "commandExecution" && kind != "mcpToolCall" && kind != "fileChange" {
		return nil
	}
	v := &agent.ToolObservation{InvocationID: text("id"), Phase: phase, Outcome: agent.ToolUnknown}
	cwd := text("cwd")
	if cwd == "" {
		cwd = workspace
	}
	switch kind {
	case "commandExecution":
		var command any
		if len(item["command"]) == 0 || string(item["command"]) == `""` || string(item["command"]) == "null" {
			break
		}
		if json.Unmarshal(item["command"], &command) != nil {
			break
		}
		validCommand := false
		switch value := command.(type) {
		case string:
			validCommand = value != ""
		case []any:
			validCommand = len(value) > 0
			for _, part := range value {
				if _, ok := part.(string); !ok {
					validCommand = false
				}
			}
		}
		if !validCommand {
			break
		}
		options := make(map[string]json.RawMessage)
		for _, key := range []string{"command", "env", "envOverrides", "stdin", "timeoutMs", "sandboxPolicy"} {
			if value, ok := item[key]; ok {
				options[key] = value
			}
		}
		v.Operation = agent.ToolFingerprint("codex", kind, cwd, options)
		if phase == agent.ToolCompleted {
			var exit *int
			_ = json.Unmarshal(item["exitCode"], &exit)
			if exit != nil {
				if *exit == 0 {
					v.Outcome = agent.ToolSucceeded
				} else {
					v.Outcome = agent.ToolFailed
				}
			}
			if output, ok := item["aggregatedOutput"]; ok {
				v.Result = agent.ToolFingerprint(exit, output)
			}
		}
	case "mcpToolCall":
		if text("server") != "" && text("tool") != "" && len(item["arguments"]) > 0 && string(item["arguments"]) != "null" {
			v.Operation = agent.ToolFingerprint("codex", kind, cwd, text("server"), text("tool"), item["arguments"])
		}
		if phase == agent.ToolCompleted {
			if failure := item["error"]; len(failure) > 0 && string(failure) != "null" {
				v.Outcome = agent.ToolFailed
				v.NonTransient = agent.PermanentProtocolError(failure)
				v.Result = agent.ToolFingerprint(failure)
			} else if result := item["result"]; len(result) > 0 && string(result) != "null" {
				var outcome struct {
					IsError bool `json:"isError"`
				}
				if json.Unmarshal(result, &outcome) == nil {
					v.Outcome = agent.ToolSucceeded
					if outcome.IsError {
						v.Outcome = agent.ToolFailed
					}
					v.Result = agent.ToolFingerprint(result)
				}
			}
		}
	case "fileChange":
		if len(item["changes"]) > 0 {
			v.Operation = agent.ToolFingerprint("codex", kind, cwd, item["changes"])
		}
		if phase == agent.ToolCompleted && text("status") == "completed" {
			v.Outcome = agent.ToolSucceeded
		}
	}
	v.Complete = v.InvocationID != "" && v.Operation != "" && cwd != ""
	return v
}

// Unknown item kinds can hide tool work or progress. They invalidate automatic
// decisions; ordinary model text and reasoning are not tool executions.
func itemNeedsEvidenceBarrier(raw json.RawMessage) bool {
	var params struct {
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return true
	}
	switch params.Item.Type {
	case "agentMessage", "userMessage", "reasoning", "plan":
		return false
	default:
		return true
	}
}

func itemBelongsToTurn(raw json.RawMessage, threadID, turnID string) bool {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return false
	}
	return (params.ThreadID == "" || params.ThreadID == threadID) && (params.TurnID == "" || params.TurnID == turnID)
}
