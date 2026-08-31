package chat

import (
	"regexp"
	"strings"
)

var (
	workIntentPattern                = regexp.MustCompile(`(?i)\b(implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|uninstall|rename|move|migrate|generate|apply|revert|restore|execute|ship|land|merge|start|proceed|continue|resume|finish)\b`)
	runWorkPattern                   = regexp.MustCompile(`(?i)\brun\s+(the\s+)?(tests?|checks?|build|formatter|migration|script|command|make\b|go\s+)`)
	directWorkPattern                = regexp.MustCompile(`(?i)^(?:(?:please|yes|ok(?:ay)?|go ahead)\s+)*(?:implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|uninstall|rename|move|migrate|generate|apply|revert|restore|execute|ship|land|merge|start|proceed|continue|resume|finish)\b|^(?:can|could|would|will)\s+you\b.*\b(?:implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|rename|move|migrate|generate|apply|execute|finish)\b|\b(?:i want you to|i would like you to|i'd like you to|let'?s|we need to|you need to|make sure to)\b.*\b(?:implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|rename|move|migrate|generate|apply|execute|finish)\b`)
	questionPattern                  = regexp.MustCompile(`(?i)^(who|what|when|where|why|how|which|is|are|was|were|do|does|did|can|could|would|should|will|tell|explain|show|list|status|help)\b`)
	informationalWorkQuestionPattern = regexp.MustCompile(`(?i)^(who|what|when|where|why|how|which|is|are|was|were|do|does|did|should\s+(?:we|i))\b`)
	quickPattern                     = regexp.MustCompile(`(?i)\b(status|stats?|statistics|setting|settings|help|command|commands|conflict count|correction count|who is|what is|how do i|how can i)\b`)
	researchPattern                  = regexp.MustCompile(`(?i)\b(research|investigate|audit|trace|inspect (the )?(files?|code|repository|repo)|search (the )?(web|internet)|look up|verify online)\b`)
	operationalStatusPatterns        = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^\s*(?:where\s+are\s+we|where\s+do\s+we\s+stand)(?:\s+(?:now|currently))?[?.!]*\s*$`),
		regexp.MustCompile(`(?i)^\s*(?:give\s+me\s+an?\s+)?status(?:\s+update)?[?.!]*\s*$`),
		regexp.MustCompile(`(?i)^\s*what(?:'s|\s+is|\s+are)\s+(?:@?[a-z][a-z0-9-]*\s+doing|running)[?.!]*\s*$`),
		regexp.MustCompile(`(?i)^\s*is\s+(?:anyone|anybody)\s+(?:stuck|blocked|waiting)[?.!]*\s*$`),
		regexp.MustCompile(`(?i)^\s*(?:is\s+there\s+)?anything\s+queued[?.!]*\s*$`),
	}
	conversationalPrefixPattern = regexp.MustCompile(`(?i)^(?:(?:no|yeah|yep|yes|well|actually|but|ok(?:ay)?|hey)[\s,.:;!?-]+)+`)
	cancellationPattern         = regexp.MustCompile(`(?i)^(?:never\s*mind|forget it|disregard that|leave it|i(?:'ll| will) handle it)\b`)
	selfDirectedWorkPattern     = regexp.MustCompile(`(?i)^i(?:'ll| will| am going to|'m going to)\b.*\b(?:implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|rename|move|migrate|generate|apply|execute|finish)\b`)
	askedWorkPattern            = regexp.MustCompile(`(?i)^i\s+(?:just\s+)?asked\s+(?:(?:you\s+to)|(?:that\s+you))\s+.*\b(?:implement|fix|change|modify|update|edit|write|create|add|remove|delete|refactor|build|commit|push|deploy|install|rename|move|migrate|generate|apply|execute|finish)\b`)
	explanationPattern          = regexp.MustCompile(`(?i)^(?:(?:can|could|would|will)\s+(?:(?:one|some|any)\s+of\s+you|someone|anyone|you)\s+)?(?:please\s+)?(?:explain|tell me|help me understand)\b`)
)

// IsOperationalStatusQuery recognizes only questions answerable from trusted
// room state. Broader interpretation remains a normal read-only conversation.
func IsOperationalStatusQuery(text string) bool {
	for _, pattern := range operationalStatusPatterns {
		if pattern.MatchString(strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

// ClassifyInput deliberately chooses ambiguity when language is not strong
// enough to authorize work. This is a deterministic host decision: room
// activity and provider availability never influence the result.
func ClassifyInput(text string, hasAttachments bool) (InputIntent, IntentConfidence, ConversationClass) {
	value := strings.TrimSpace(text)
	lower := strings.ToLower(value)
	normalized := strings.TrimSpace(conversationalPrefixPattern.ReplaceAllString(lower, ""))
	class := ConversationStandard
	if quickPattern.MatchString(lower) && !researchPattern.MatchString(lower) {
		class = ConversationQuick
	}
	if researchPattern.MatchString(lower) || hasAttachments {
		class = ConversationResearch
	}
	if cancellationPattern.MatchString(lower) || selfDirectedWorkPattern.MatchString(normalized) || explanationPattern.MatchString(normalized) {
		return InputConversation, IntentHigh, class
	}
	if informationalWorkQuestionPattern.MatchString(normalized) {
		return InputConversation, IntentHigh, class
	}
	if directWorkPattern.MatchString(normalized) || runWorkPattern.MatchString(normalized) || askedWorkPattern.MatchString(normalized) {
		return InputWork, IntentHigh, class
	}
	if strings.HasSuffix(value, "?") || questionPattern.MatchString(normalized) || researchPattern.MatchString(lower) ||
		strings.HasPrefix(lower, "thanks") || strings.HasPrefix(lower, "thank you") ||
		strings.HasPrefix(lower, "hello") || strings.HasPrefix(lower, "hi ") {
		return InputConversation, IntentHigh, class
	}
	if workIntentPattern.MatchString(lower) {
		return InputAmbiguous, IntentLow, class
	}
	return InputAmbiguous, IntentLow, class
}
