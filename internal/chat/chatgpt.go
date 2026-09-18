package chat

import (
	"fmt"
	"time"
)

// ChatGPTLimits are host-owned, room-specific participation settings. They do
// not grant filesystem permissions or change the room's rate/concurrency caps.
type ChatGPTLimits struct {
	Exchanges        int `json:"exchanges"`
	FollowUps        int `json:"follow_ups"`
	FollowUpSeconds  int `json:"follow_up_seconds"`
	RepeatedRequests int `json:"repeated_requests"`
}

func DefaultChatGPTLimits() ChatGPTLimits {
	return ChatGPTLimits{Exchanges: 32, FollowUps: 32, FollowUpSeconds: 3600, RepeatedRequests: 3}
}

func (l ChatGPTLimits) Validate() error {
	if l.Exchanges < 1 || l.Exchanges > 1000 || l.FollowUps < 1 || l.FollowUps > 1000 {
		return fmt.Errorf("ChatGPT exchanges and followups must each be between 1 and 1000")
	}
	if l.FollowUpSeconds < 60 || l.FollowUpSeconds > 86400 {
		return fmt.Errorf("ChatGPT follow-up duration must be between 1m and 24h")
	}
	if l.RepeatedRequests < 2 || l.RepeatedRequests > 20 {
		return fmt.Errorf("ChatGPT repeats must be between 2 and 20")
	}
	return nil
}

// ChatGPTState is safe to display to room participants. It contains no grant,
// connection path, private conversation, or participation capability.
type ChatGPTState struct {
	Enabled   bool `json:"enabled"`
	Connected bool `json:"connected"`
	// Paused means an explicit host stop. Budget/loop pauses only prevent new
	// requests; reading results and publishing a final summary remain available.
	Paused             bool          `json:"paused"`
	ExpiresAt          time.Time     `json:"expires_at"`
	LeaseUntil         time.Time     `json:"lease_until"`
	ExchangesRemaining int           `json:"exchanges_remaining"`
	Limits             ChatGPTLimits `json:"limits"`
	PauseReason        string        `json:"pause_reason,omitempty"`
}
