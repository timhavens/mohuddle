package agent

import (
	"encoding/json"
	"math"
)

// ToolTracker joins providers whose result frames contain only a call ID to
// the input fingerprint from their start frame. It never stores raw inputs.
// Caller serializes access; overflow deliberately returns incomplete evidence.
type ToolTracker struct{ starts map[string]ToolObservation }

func (t *ToolTracker) Start(id, provider, name, workspace string, arguments any) *ToolObservation {
	v := &ToolObservation{InvocationID: id, Phase: ToolStarted, Outcome: ToolUnknown}
	raw, err := json.Marshal(arguments)
	if name != "" && err == nil && string(raw) != "null" && !uncertainToolNumber(arguments) {
		v.Operation = ToolFingerprint(provider, name, workspace, arguments)
	}
	v.Complete = id != "" && v.Operation != ""
	if t.starts == nil {
		t.starts = make(map[string]ToolObservation)
	}
	if _, found := t.starts[id]; found || len(t.starts) < 256 {
		// Preserve the first start so conflicting duplicate starts cannot replace
		// the evidence used to correlate its completion.
		if !found {
			t.starts[id] = *v
		}
	} else {
		v.Complete = false
	}
	return v
}

// Some SDKs decode arbitrary JSON into float64. Large integers can already
// have lost identity by then; do not pretend those rounded inputs are exact.
func uncertainToolNumber(value any) bool {
	switch v := value.(type) {
	case float64:
		return math.Abs(v) >= 1<<53
	case []any:
		for _, x := range v {
			if uncertainToolNumber(x) {
				return true
			}
		}
	case map[string]any:
		for _, x := range v {
			if uncertainToolNumber(x) {
				return true
			}
		}
	}
	return false
}

func (t *ToolTracker) Finish(id string, outcome ToolOutcome, result any) *ToolObservation {
	v := t.starts[id]
	v.InvocationID, v.Phase, v.Outcome = id, ToolCompleted, outcome
	if result != nil {
		v.Result = ToolFingerprint(result)
	}
	return &v
}
