package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/timhavens/mohuddle/internal/chat"
)

func TestWithParticipantRewritesProviderIdentity(t *testing.T) {
	base := &instanceTestAgent{participant: chat.Codex}
	assigned := chat.Participant("codex-1")
	wrapped, err := WithParticipant(base, assigned)
	if err != nil {
		t.Fatal(err)
	}
	if got := wrapped.Participant(); got != assigned {
		t.Fatalf("Participant()=%q want=%q", got, assigned)
	}

	var emitted Event
	_, err = wrapped.Run(context.Background(), TurnRequest{Prompt: "test"}, func(event Event) { emitted = event })
	if err == nil {
		t.Fatal("Run() error=nil, want availability error")
	}
	if emitted.Agent != assigned || emitted.Approval == nil || emitted.Approval.Agent != assigned {
		t.Fatalf("emitted event=%+v approval=%+v", emitted, emitted.Approval)
	}
	if base.approval.Agent != chat.Codex {
		t.Fatalf("wrapper mutated provider approval: %+v", base.approval)
	}
	var unavailable *AvailabilityError
	if !errors.As(err, &unavailable) || unavailable.Participant != assigned || unavailable.Reason != "quota" {
		t.Fatalf("Run() error=%T %+v", err, err)
	}
	if base.unavailable.Participant != chat.Codex {
		t.Fatalf("wrapper mutated provider error: %+v", base.unavailable)
	}
}

func TestWithParticipantDelegatesOptionalCapabilitiesAndClose(t *testing.T) {
	base := &instanceTestAgent{participant: chat.Claude}
	wrapped, err := WithParticipant(base, "claude-2")
	if err != nil {
		t.Fatal(err)
	}
	configurable, ok := wrapped.(Configurable)
	if !ok {
		t.Fatal("wrapped agent does not expose Configurable")
	}
	wantSettings := chat.AgentSettings{Model: "model", Effort: "high", Permissions: chat.PermissionReadOnly}
	if !configurable.Configure(wantSettings) || base.settings != wantSettings {
		t.Fatalf("Configure was not delegated: configured=%+v", base.settings)
	}
	catalog, ok := wrapped.(ModelCatalog)
	if !ok {
		t.Fatal("wrapped agent does not expose ModelCatalog")
	}
	models, err := catalog.Models(context.Background())
	if err != nil || !reflect.DeepEqual(models, base.models) || base.modelCalls != 1 {
		t.Fatalf("Models()=(%+v, %v), calls=%d", models, err, base.modelCalls)
	}
	if err := wrapped.Close(); err != nil || base.closeCalls != 1 {
		t.Fatalf("Close()=%v calls=%d", err, base.closeCalls)
	}
}

func TestWithParticipantValidatesProviderAndPreservesPrimary(t *testing.T) {
	base := &instanceTestAgent{participant: chat.Codex}
	if got, err := WithParticipant(base, chat.Codex); err != nil || got != base {
		t.Fatalf("primary WithParticipant()=(%T, %v), want original", got, err)
	}
	for _, participant := range []chat.Participant{"claude-1", "codex-0", chat.User, "unknown-1"} {
		if got, err := WithParticipant(base, participant); err == nil || got != nil {
			t.Fatalf("WithParticipant(%q)=(%T, %v), want rejection", participant, got, err)
		}
	}
	if got, err := WithParticipant(nil, "codex-1"); err == nil || got != nil {
		t.Fatalf("WithParticipant(nil)=(%T, %v), want rejection", got, err)
	}
}

type instanceTestAgent struct {
	participant chat.Participant
	approval    *ApprovalRequest
	unavailable *AvailabilityError
	settings    chat.AgentSettings
	models      []ModelOption
	modelCalls  int
	closeCalls  int
}

func (a *instanceTestAgent) Participant() chat.Participant { return a.participant }

func (a *instanceTestAgent) Run(_ context.Context, _ TurnRequest, emit func(Event)) (TurnResult, error) {
	a.approval = &ApprovalRequest{Agent: a.participant, Kind: "tool", Title: "approve"}
	emit(Event{Type: EventApproval, Agent: a.participant, Approval: a.approval})
	a.unavailable = &AvailabilityError{Participant: a.participant, Reason: "quota"}
	return TurnResult{}, a.unavailable
}

func (a *instanceTestAgent) Configure(value chat.AgentSettings) bool {
	a.settings = value
	return true
}

func (a *instanceTestAgent) Models(context.Context) ([]ModelOption, error) {
	a.modelCalls++
	if a.models == nil {
		a.models = []ModelOption{{ID: "model", Name: "Model", Default: true}}
	}
	return append([]ModelOption(nil), a.models...), nil
}

func (a *instanceTestAgent) Close() error {
	a.closeCalls++
	return nil
}
