package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

type ToolPhase string
type ToolOutcome string

const (
	ToolStarted   ToolPhase   = "started"
	ToolCompleted ToolPhase   = "completed"
	ToolUnknown   ToolOutcome = "unknown"
	ToolSucceeded ToolOutcome = "succeeded"
	ToolFailed    ToolOutcome = "failed"
)

// ToolObservation is process-local evidence. Fingerprints contain no display
// text and must never be persisted, exported, or substituted with summaries.
// A nonzero command exit is a failure outcome, NOT proof of a stuck workflow.
type ToolObservation struct {
	InvocationID string
	Operation    string
	Phase        ToolPhase
	Outcome      ToolOutcome
	Result       string
	Complete     bool
	// NonTransient is only set for explicit structured protocol errors. Human
	// error text, shell exit codes, and generic is_error flags are insufficient.
	NonTransient bool
}

// ToolFingerprint canonicalizes JSON object ordering, preserving numbers and
// strings exactly (including shell case, whitespace, and argument boundaries).
func ToolFingerprint(values ...any) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var canonical any
	if decoder.Decode(&canonical) != nil {
		return ""
	}
	encoded, err = json.Marshal(canonical)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

// PermanentProtocolError deliberately excludes timeouts, permission/quota
// states, application errors, and prose. It does not infer retryability.
func PermanentProtocolError(raw json.RawMessage) bool {
	var value struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value.Code == -32600 || value.Code == -32601 || value.Code == -32602
}
