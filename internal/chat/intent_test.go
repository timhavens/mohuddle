package chat

import (
	"testing"
	"time"
)

func TestClassifyInputIsIndependentOfRoomState(t *testing.T) {
	tests := []struct {
		text   string
		intent InputIntent
		class  ConversationClass
	}{
		{"how do I see the conflict stats?", InputConversation, ConversationQuick},
		{"what is that about?", InputConversation, ConversationQuick},
		{"please implement the plan now", InputWork, ConversationStandard},
		{"can you fix the Esc shortcut?", InputWork, ConversationStandard},
		{"how hard would this be to implement?", InputConversation, ConversationStandard},
		{"what should we add next?", InputConversation, ConversationStandard},
		{"do we need to fix this now?", InputConversation, ConversationStandard},
		{"the fix failed again; what happened?", InputConversation, ConversationStandard},
		{"the fix failed again", InputAmbiguous, ConversationStandard},
		{"we need to fix the restart path", InputWork, ConversationStandard},
		{"run make check, commit, and push", InputWork, ConversationStandard},
		{"this deserves another look", InputAmbiguous, ConversationStandard},
		{"investigate the repository and explain the cause", InputConversation, ConversationResearch},
	}
	for _, test := range tests {
		intent, _, class := ClassifyInput(test.text, false)
		if intent != test.intent || class != test.class {
			t.Errorf("ClassifyInput(%q)=(%q,%q), want (%q,%q)", test.text, intent, class, test.intent, test.class)
		}
	}
}

func TestOperationalStatusRecognizerIsNarrowAndDeterministic(t *testing.T) {
	for _, value := range []string{"where are we?", "status update", "what is @claude doing", "what's running?", "is anyone stuck", "anything queued?"} {
		if !IsOperationalStatusQuery(value) {
			t.Errorf("status query was not recognized: %q", value)
		}
	}
	for _, value := range []string{"what is this code doing?", "why is anyone stuck on parsing?", "research provider status semantics"} {
		if IsOperationalStatusQuery(value) {
			t.Errorf("interpretive question was misclassified as host status: %q", value)
		}
	}
}

func TestConversationClassBudgetsAreLongFailureWindows(t *testing.T) {
	if ConversationQuick.TotalBudget() != 10*time.Minute || ConversationStandard.TotalBudget() != 10*time.Minute || ConversationResearch.TotalBudget() != 30*time.Minute {
		t.Fatalf("budgets: quick=%s standard=%s research=%s", ConversationQuick.TotalBudget(), ConversationStandard.TotalBudget(), ConversationResearch.TotalBudget())
	}
	if ConversationQuick.AttemptBudget() != ConversationQuick.TotalBudget() {
		t.Fatalf("attempt budget=%s total=%s", ConversationQuick.AttemptBudget(), ConversationQuick.TotalBudget())
	}
}
