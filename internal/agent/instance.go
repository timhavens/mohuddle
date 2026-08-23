package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/timhavens/mohuddle/internal/chat"
)

// WithParticipant gives a provider adapter a distinct room identity while
// retaining the adapter's provider implementation. Events, approvals, and
// availability errors are rewritten to the assigned identity.
func WithParticipant(base Agent, participant chat.Participant) (Agent, error) {
	if base == nil || !participant.ValidAgent() || participant.Provider() != base.Participant().Provider() {
		return nil, fmt.Errorf("participant %q does not match provider adapter", participant)
	}
	if participant == base.Participant() {
		return base, nil
	}
	return &instance{base: base, participant: participant}, nil
}

type instance struct {
	base        Agent
	participant chat.Participant
}

func (i *instance) Participant() chat.Participant { return i.participant }

func (i *instance) Run(ctx context.Context, request TurnRequest, emit func(Event)) (TurnResult, error) {
	result, err := i.base.Run(ctx, request, func(event Event) {
		event.Agent = i.participant
		if event.Approval != nil {
			copy := *event.Approval
			copy.Agent = i.participant
			event.Approval = &copy
		}
		emit(event)
	})
	var unavailable *AvailabilityError
	if errors.As(err, &unavailable) {
		copy := *unavailable
		copy.Participant = i.participant
		err = &copy
	}
	return result, err
}

func (i *instance) Configure(value chat.AgentSettings) bool {
	configurable, ok := i.base.(Configurable)
	return ok && configurable.Configure(value)
}

func (i *instance) Models(ctx context.Context) ([]ModelOption, error) {
	catalog, ok := i.base.(ModelCatalog)
	if !ok {
		return nil, fmt.Errorf("model catalog is unavailable for %s", i.participant)
	}
	return catalog.Models(ctx)
}

func (i *instance) Close() error { return i.base.Close() }
