package room

import (
	"fmt"

	"github.com/timhavens/mohuddle/internal/agent"
)

const maxTrackedToolExecutions = 256

type toolExecution struct {
	start      agent.ToolObservation
	end        agent.ToolObservation
	done       bool
	overlapped bool
}

type toolAttempt struct {
	signature string
	eligible  bool
}

type workflowLoopMonitor struct {
	workflowID string
	executions map[string]*toolExecution
	inFlight   int
	attempts   []toolAttempt
	warned     string
	disabled   bool
}

type loopDecision struct {
	recover bool
	reason  string
}

func (m *workflowLoopMonitor) resetSequence() { m.attempts = nil; m.warned = "" }

func (m *workflowLoopMonitor) observe(value *agent.ToolObservation) loopDecision {
	if value == nil {
		m.disabled = true
		m.resetSequence()
		return loopDecision{}
	}
	v := *value
	if m.disabled {
		return loopDecision{}
	}
	if v.InvocationID == "" || v.Operation == "" || !v.Complete {
		m.disabled = true
		m.resetSequence()
		return loopDecision{}
	}
	if m.executions == nil {
		m.executions = make(map[string]*toolExecution)
	}
	existing := m.executions[v.InvocationID]
	if v.Phase == agent.ToolStarted {
		if existing != nil {
			if existing.start != v {
				m.disabled = true
				m.resetSequence()
			}
			return loopDecision{}
		}
		if len(m.executions) >= maxTrackedToolExecutions {
			m.disabled = true
			m.resetSequence()
			return loopDecision{reason: "tool tracking limit reached; loop detection is advisory for this turn and work continues"}
		}
		e := &toolExecution{start: v, overlapped: m.inFlight > 0}
		if e.overlapped {
			m.resetSequence()
			for _, other := range m.executions {
				if !other.done {
					other.overlapped = true
				}
			}
		}
		m.executions[v.InvocationID] = e
		m.inFlight++
		return loopDecision{}
	}
	if v.Phase != agent.ToolCompleted || existing == nil {
		// Without the start we cannot establish sequential execution. Remember
		// the uncertainty for this turn rather than fabricate an execution.
		m.disabled = true
		m.resetSequence()
		return loopDecision{}
	}
	if existing.done {
		if existing.end != v {
			m.disabled = true
			m.resetSequence()
		}
		return loopDecision{}
	}
	existing.done, existing.end = true, v
	m.inFlight--
	if existing.start.Operation != v.Operation || existing.overlapped || m.inFlight != 0 || v.Outcome != agent.ToolFailed || v.Result == "" {
		m.resetSequence()
		return loopDecision{}
	}
	m.attempts = append(m.attempts, toolAttempt{signature: agent.ToolFingerprint(v.Operation, v.Outcome, v.Result), eligible: v.NonTransient})
	if len(m.attempts) > 12 {
		m.attempts = m.attempts[len(m.attempts)-12:]
	}
	for size := 1; size <= 4; size++ {
		length := size * 3
		if len(m.attempts) < length {
			continue
		}
		tail := m.attempts[len(m.attempts)-length:]
		match, eligible := true, true
		for i, attempt := range tail {
			eligible = eligible && attempt.eligible
			if attempt.signature != tail[i%size].signature {
				match = false
			}
		}
		if !match {
			continue
		}
		keys := make([]string, size)
		for i := range size {
			keys[i] = tail[i].signature
		}
		// A/B/A/B and B/A/B/A are the same unchanged cycle, not new warnings.
		warning := agent.ToolFingerprint(keys)
		for offset := 1; offset < size; offset++ {
			rotated := append(append([]string(nil), keys[offset:]...), keys[:offset]...)
			if key := agent.ToolFingerprint(rotated); key < warning {
				warning = key
			}
		}
		if m.warned == warning {
			return loopDecision{}
		}
		m.warned = warning
		reason := "three sequential completed attempts returned the same failure"
		if size > 1 {
			reason = fmt.Sprintf("a cycle of %d sequential completed failures repeated three times", size)
		}
		if eligible {
			return loopDecision{recover: true, reason: reason + "; structured protocol errors identify a non-transient failure"}
		}
		return loopDecision{reason: reason + "; retry intent is uncertain, so work continues"}
	}
	return loopDecision{}
}

func (o *Orchestrator) observeWorkflowTool(workflowID, turnID string, value *agent.ToolObservation) loopDecision {
	if workflowID == "" || turnID == "" {
		return loopDecision{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	m := o.loopMonitors[turnID]
	if m == nil {
		m = &workflowLoopMonitor{workflowID: workflowID}
		o.loopMonitors[turnID] = m
	}
	return m.observe(value)
}
