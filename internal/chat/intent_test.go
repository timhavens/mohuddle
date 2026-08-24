package chat

import "testing"

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
