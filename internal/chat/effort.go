package chat

import (
	"maps"
	"time"
)

// EffortSelection belongs to one accepted operation, never to room settings.
type EffortSelection struct {
	Efforts map[Participant]string `json:"efforts,omitempty"`
	Reason  string                 `json:"effort_reason,omitempty"`
}

func (e EffortSelection) Clone() EffortSelection {
	e.Efforts = maps.Clone(e.Efforts)
	return e
}

func (e EffortSelection) Equal(other EffortSelection) bool {
	return e.Reason == other.Reason && maps.Equal(e.Efforts, other.Efforts)
}

// EffortStatus distinguishes an override, what the host sent, and provider facts.
// Empty ReportedEffort means unconfirmed; it must not be filled from a request.
type EffortStatus struct {
	RequestedEffort string    `json:"requested_effort,omitempty"`
	AppliedEffort   string    `json:"applied_effort,omitempty"`
	Model           string    `json:"model,omitempty"`
	ReportedEffort  string    `json:"reported_effort,omitempty"`
	ReportedModel   string    `json:"reported_model,omitempty"`
	ReportSource    string    `json:"report_source,omitempty"`
	ConfirmedAt     time.Time `json:"confirmed_at,omitzero"`
	Error           string    `json:"error,omitempty"`
}

type EffortCapability struct {
	Participant      Participant `json:"participant"`
	Provider         Participant `json:"provider"`
	Model            string      `json:"model,omitempty"`
	StandingEffort   string      `json:"standing_effort"`
	AvailableEfforts []string    `json:"available_efforts"`
	CapabilitySource string      `json:"capability_source"`
	DiscoveryPending bool        `json:"discovery_pending,omitempty"`
}

// EffortError is a local request/configuration failure, not provider unavailability.
type EffortError struct{ Message string }

func (e *EffortError) Error() string { return e.Message }
