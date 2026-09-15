package chat

import "time"

// ChatGPTState is safe to display to room participants. It contains no grant,
// connection path, private conversation, or participation capability.
type ChatGPTState struct {
	Enabled            bool      `json:"enabled"`
	Connected          bool      `json:"connected"`
	Paused             bool      `json:"paused"`
	ExpiresAt          time.Time `json:"expires_at"`
	LeaseUntil         time.Time `json:"lease_until"`
	ExchangesRemaining int       `json:"exchanges_remaining"`
}
