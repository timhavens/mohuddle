package agent

// EventQueueOverflowError identifies a local transport capacity failure, not
// provider unavailability. It deliberately carries no provider payload.
type EventQueueOverflowError struct{}

func (*EventQueueOverflowError) Error() string { return "codex event queue overflow" }
