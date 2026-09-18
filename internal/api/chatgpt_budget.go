package api

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

// SetChatGPTLimits is a trusted local control, deliberately absent from the
// external ChatGPT protocol. Changing limits preserves usage and host pauses.
func (s *Service) SetChatGPTLimits(limits chat.ChatGPTLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	s.chatgpt.limits = limits
	s.updateChatGPTStateLocked()
	return nil
}

func (a *chatGPTAccess) effectiveLimits() chat.ChatGPTLimits {
	if a.limits == (chat.ChatGPTLimits{}) {
		return chat.DefaultChatGPTLimits()
	}
	return a.limits
}

func (a *chatGPTAccess) resumeBudget() {
	a.exchanges = 0
	a.repeated = nil
	a.noProgress = false
}

// Distinct public peer text and successful work completion are observable
// progress, not proof of correctness. Polls, self posts, repeated answer text,
// failures, and freshly generated operation IDs are not progress. This is a
// bounded repetition heuristic, not a semantic assessment of task completion.
func (a *chatGPTAccess) observeProgress(state chat.Room, messages []chat.Message) {
	if a.progressText == nil {
		a.progressText = make(map[[32]byte]struct{})
	}
	progress := false
	for _, message := range messages {
		if message.Author == chat.User && message.Route == nil && message.Sequence > a.human {
			a.human = message.Sequence
			a.resumeBudget()
		}
		if message.Sequence <= a.progressAfter {
			continue
		}
		a.progressAfter = message.Sequence
		if message.Kind != chat.MessageText || !message.Author.ValidAgent() || strings.TrimSpace(message.Text) == "" {
			continue
		}
		key := sha256.Sum256([]byte(strings.Join(strings.Fields(message.Text), " ")))
		if _, seen := a.progressText[key]; !seen {
			progress = true
			// Bound memory independently of the lifetime of an active grant.
			if len(a.progressText) >= 4096 {
				a.progressText = make(map[[32]byte]struct{})
			}
			a.progressText[key] = struct{}{}
		}
	}
	completed := make(map[string]struct{})
	workSeen := 0
	for i := len(messages) - 1; i >= 0 && workSeen < 4096; i-- {
		message := messages[i]
		if message.Author != chat.ChatGPT || !message.IsWorkflowSource() {
			continue
		}
		workSeen++
		if record, ok := state.Workflows[message.WorkflowID]; ok && record.State == chat.WorkflowCompleted {
			if _, seen := a.completedWork[record.ID]; !seen {
				progress = true
			}
			completed[record.ID] = struct{}{}
		}
	}
	a.completedWork = completed
	if progress {
		a.repeated = nil
	}
}

func chatGPTRequestFingerprint(action, text string, targets []chat.Participant) [32]byte {
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		normalized = append(normalized, strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(target)), "@")))
	}
	slices.Sort(normalized)
	// Do not include operation_id or reply_to: changing bookkeeping cannot
	// turn the same instruction into new work. Preserve case within task text.
	data, _ := json.Marshal([]any{action, strings.Join(strings.Fields(text), " "), normalized})
	return sha256.Sum256(data)
}

func (a *chatGPTAccess) requestPauseReason() string {
	if a.noProgress {
		return "no_progress"
	}
	if a.exchanges >= a.effectiveLimits().Exchanges {
		return "exchange_limit"
	}
	return ""
}

func (s *Service) chatGPTBudgetFailure(request Request, code, message string) HandleResult {
	result := failed(request, code, message)
	result.Response.Result = map[string]any{
		"message_posted": false, "agent_scheduled": false, "scheduled_agents": []chat.Participant{},
		"exchanges_remaining": max(0, s.chatgpt.effectiveLimits().Exchanges-s.chatgpt.exchanges),
		"limits":              s.chatgpt.effectiveLimits(), "pause_reason": code,
	}
	return result
}
