package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// ProposedPlan is the host-owned snapshot shown in the Codex-style plan
// completion prompt. Content is stored separately from transcript-window
// context so an approved implementation always receives the exact plan.
type ProposedPlan struct {
	ID              string      `json:"id"`
	WorkflowID      string      `json:"workflow_id,omitempty"`
	SourceMessageID string      `json:"source_message_id"`
	SourceSequence  uint64      `json:"source_sequence"`
	Author          Participant `json:"author"`
	Content         string      `json:"content"`
	SHA256          string      `json:"sha256"`
	CreatedAt       time.Time   `json:"created_at"`
}

var (
	proposedPlanPattern = regexp.MustCompile(`(?s)<proposed_plan>[\t\r\n ]*(.*?)[\t\r\n ]*</proposed_plan>`)
	proposedPlanOpen    = regexp.MustCompile(`<proposed_plan>`)
	proposedPlanClose   = regexp.MustCompile(`</proposed_plan>`)
)

// ExtractProposedPlan accepts exactly one non-empty terminal plan block. A
// quoted example or malformed duplicate cannot become executable state.
func ExtractProposedPlan(value string) (string, bool) {
	if len(proposedPlanOpen.FindAllStringIndex(value, -1)) != 1 || len(proposedPlanClose.FindAllStringIndex(value, -1)) != 1 {
		return "", false
	}
	match := proposedPlanPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", false
	}
	content := strings.TrimSpace(match[1])
	if content == "" {
		return "", false
	}
	after := value[proposedPlanPattern.FindStringIndex(value)[1]:]
	if strings.TrimSpace(after) != "" {
		return "", false
	}
	return content, true
}

func ProposedPlanHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func (p ProposedPlan) Valid() bool {
	return strings.TrimSpace(p.ID) != "" && strings.TrimSpace(p.SourceMessageID) != "" &&
		p.SourceSequence != 0 && p.Author.ValidAgent() && strings.TrimSpace(p.Content) != "" &&
		p.SHA256 == ProposedPlanHash(p.Content) && !p.CreatedAt.IsZero()
}

// DisplayProposedPlan removes the transport tags while retaining surrounding
// explanatory text. The TUI supplies the special Plan rendering.
func DisplayProposedPlan(value string) (string, bool) {
	content, ok := ExtractProposedPlan(value)
	if !ok {
		return value, false
	}
	location := proposedPlanPattern.FindStringIndex(value)
	prefix := strings.TrimSpace(value[:location[0]])
	if prefix == "" {
		return content, true
	}
	return prefix + "\n\n" + content, true
}
