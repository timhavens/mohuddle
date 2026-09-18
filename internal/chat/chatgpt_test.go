package chat

import "testing"

func TestChatGPTLimitsRejectUnboundedOrAmbiguousValues(t *testing.T) {
	for _, field := range []string{"exchanges", "followups", "duration", "repeats"} {
		for _, value := range []int{-1, 0, 100001} {
			limits := DefaultChatGPTLimits()
			switch field {
			case "exchanges":
				limits.Exchanges = value
			case "followups":
				limits.FollowUps = value
			case "duration":
				limits.FollowUpSeconds = value
			case "repeats":
				limits.RepeatedRequests = value
			}
			if limits.Validate() == nil {
				t.Fatalf("accepted %s=%d", field, value)
			}
		}
	}
	for _, limits := range []ChatGPTLimits{DefaultChatGPTLimits(), {1, 1, 60, 2}, {1000, 1000, 86400, 20}} {
		if err := limits.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
