package room

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
)

type effortModelKey struct {
	provider chat.Participant
	model    string
}
type effortCatalogEntry struct {
	model  string
	levels []string
}
type effortDiscovery struct {
	loading bool
	checked time.Time
}

// EffortCapabilities never waits for provider discovery. A bounded background
// catalog lookup improves subsequent reads and dispatch validation; unavailable
// catalogs retain explicitly labelled provider-level validation.
func (o *Orchestrator) EffortCapabilities() []chat.EffortCapability {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]chat.EffortCapability, 0)
	for _, participant := range o.room.PresentAgents() {
		o.refreshEffortCatalogLocked(participant)
		result = append(result, o.effortCapabilityLocked(participant))
	}
	return result
}

func (o *Orchestrator) effortCapabilityLocked(participant chat.Participant) chat.EffortCapability {
	settings := effectiveRoleSettings(participant, o.settings[participant])
	standing := settings.Effort
	if standing == "" {
		standing = "auto"
	}
	result := chat.EffortCapability{Participant: participant, Provider: participant.Provider(), Model: settings.Model,
		StandingEffort: standing, CapabilitySource: "provider_validation", AvailableEfforts: []string{},
		DiscoveryPending: o.effortDiscovery[participant.Provider()].loading}
	for _, level := range []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		value := settings
		value.Effort = level
		if appsettings.ValidateFor(participant, value) == nil {
			result.AvailableEfforts = append(result.AvailableEfforts, level)
		}
	}
	if entry, ok := o.effortCatalog[effortModelKey{participant.Provider(), settings.Model}]; ok {
		result.Model, result.CapabilitySource = entry.model, "model_catalog"
		result.AvailableEfforts = []string{"auto"}
		for _, level := range entry.levels {
			value := settings
			value.Effort = level
			if level != "" && appsettings.ValidateFor(participant, value) == nil && !slices.Contains(result.AvailableEfforts, level) {
				result.AvailableEfforts = append(result.AvailableEfforts, level)
			}
		}
	}
	return result
}

func (o *Orchestrator) refreshEffortCatalogLocked(participant chat.Participant) {
	provider := participant.Provider()
	last := o.effortDiscovery[provider]
	if o.closed || last.loading || (!last.checked.IsZero() && time.Since(last.checked) < 5*time.Minute) {
		return
	}
	catalog, ok := o.agents[participant].(agent.ModelCatalog)
	if !ok {
		return
	}
	if o.effortDiscovery == nil {
		o.effortDiscovery = make(map[chat.Participant]effortDiscovery)
	}
	o.effortDiscovery[provider] = effortDiscovery{loading: true, checked: last.checked}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		ctx, cancel := context.WithTimeout(o.lifetime, 4*time.Second)
		defer cancel()
		models, err := catalog.Models(ctx)
		o.mu.Lock()
		defer o.mu.Unlock()
		o.effortDiscovery[provider] = effortDiscovery{checked: time.Now()}
		if err == nil {
			o.cacheEffortCatalogLocked(provider, models)
		}
	}()
}

func (o *Orchestrator) cacheEffortCatalogLocked(provider chat.Participant, models []agent.ModelOption) {
	if o.effortCatalog == nil {
		o.effortCatalog = make(map[effortModelKey]effortCatalogEntry)
	}
	for key := range o.effortCatalog {
		if key.provider == provider {
			delete(o.effortCatalog, key)
		}
	}
	for _, model := range models {
		if !model.EffortsKnown {
			continue
		}
		entry := effortCatalogEntry{model: model.ID, levels: append([]string(nil), model.Efforts...)}
		o.effortCatalog[effortModelKey{provider, model.ID}] = entry
		if model.Default {
			o.effortCatalog[effortModelKey{provider, ""}] = entry
		}
	}
}

func normalizeEffortSelection(options []chat.EffortSelection) (chat.EffortSelection, error) {
	if len(options) == 0 {
		return chat.EffortSelection{}, nil
	}
	if len(options) != 1 {
		return chat.EffortSelection{}, &chat.EffortError{Message: "provide one effort selection per operation"}
	}
	input := options[0]
	result := chat.EffortSelection{Reason: strings.TrimSpace(input.Reason)}
	if len(result.Reason) > 512 || !utf8.ValidString(result.Reason) {
		return result, &chat.EffortError{Message: "effort_reason must contain at most 512 UTF-8 bytes"}
	}
	if len(input.Efforts) > 5 {
		return result, &chat.EffortError{Message: "efforts may name at most five participants"}
	}
	for key, value := range input.Efforts {
		participant := chat.Participant(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(key)), "@")))
		level := strings.ToLower(strings.TrimSpace(value))
		if !participant.ValidAgent() || level == "" {
			return result, &chat.EffortError{Message: "efforts must map local AI participants to nonempty effort levels"}
		}
		if result.Efforts == nil {
			result.Efforts = make(map[chat.Participant]string)
		}
		if _, exists := result.Efforts[participant]; exists {
			return result, &chat.EffortError{Message: "efforts contains duplicate participant names"}
		}
		result.Efforts[participant] = level
	}
	if result.Reason != "" && len(result.Efforts) == 0 {
		return result, &chat.EffortError{Message: "effort_reason requires an explicit effort selection"}
	}
	return result, nil
}

func (o *Orchestrator) validateEffortSelectionLocked(selection chat.EffortSelection, participants []chat.Participant) error {
	for participant, level := range selection.Efforts {
		if !slices.Contains(participants, participant) {
			return &chat.EffortError{Message: fmt.Sprintf("effort for %s is outside this operation's participants", participant)}
		}
		o.refreshEffortCatalogLocked(participant)
		if err := o.validateEffortLocked(participant, level); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) validateEffortLocked(participant chat.Participant, level string) error {
	capability := o.effortCapabilityLocked(participant)
	if !slices.Contains(capability.AvailableEfforts, level) {
		return &chat.EffortError{Message: fmt.Sprintf("effort %q is not supported for %s model %q (%s); available: %s", level, participant, capability.Model, capability.CapabilitySource, strings.Join(capability.AvailableEfforts, ", "))}
	}
	return nil
}

func (o *Orchestrator) turnEffortSelectionLocked(spec turnSpec) chat.EffortSelection {
	if spec.private {
		return chat.EffortSelection{}
	}
	if spec.conversationID != "" {
		if job := o.conversationLocked(spec.conversationID); job != nil {
			return job.EffortSelection
		}
	}
	return o.room.Workflows[spec.workflowID].EffortSelection
}

func (o *Orchestrator) turnEffortSettingsLocked(participant chat.Participant, spec turnSpec) (chat.AgentSettings, chat.EffortStatus) {
	settings, _ := o.turnAccessLocked(participant, spec)
	selection := o.turnEffortSelectionLocked(spec)
	status := chat.EffortStatus{RequestedEffort: selection.Efforts[participant], Model: settings.Model}
	if status.RequestedEffort != "" {
		settings.Effort = status.RequestedEffort
	}
	status.AppliedEffort = settings.Effort
	if status.AppliedEffort == "" {
		status.AppliedEffort = "auto"
	}
	return settings, status
}

// Called with the participant execution gate held, before transcript selection.
// Configure may reset native state, so its reset must also reset the host cursor.
func (o *Orchestrator) prepareEffortTurn(participant chat.Participant, spec turnSpec, runner agent.Agent) (chat.EffortStatus, error) {
	o.mu.Lock()
	settings, status := o.turnEffortSettingsLocked(participant, spec)
	var err error
	if status.RequestedEffort != "" {
		err = o.validateEffortLocked(participant, status.RequestedEffort)
	}
	o.mu.Unlock()
	if err != nil {
		status.Error = err.Error()
		status.AppliedEffort = ""
		return status, err
	}
	if configurable, ok := runner.(agent.Configurable); ok && configurable.Configure(settings) {
		o.mu.Lock()
		o.room.Sessions[participant] = chat.AgentSession{}
		delete(o.room.ParticipantRuntime, participant)
		o.mu.Unlock()
	}
	return status, nil
}

func confirmedEffort(status chat.EffortStatus, result agent.TurnResult) chat.EffortStatus {
	status.ReportedEffort, status.ReportedModel = strings.TrimSpace(result.RuntimeEffort), strings.TrimSpace(result.RuntimeModel)
	if status.ReportedEffort != "" || status.ReportedModel != "" {
		status.ReportSource, status.ConfirmedAt = strings.TrimSpace(result.RuntimeSource), time.Now().UTC()
	}
	return status
}

func (o *Orchestrator) recordWorkflowEffort(participant chat.Participant, spec turnSpec, status chat.EffortStatus) {
	if spec.private || spec.workflowID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if record, ok := o.room.Workflows[spec.workflowID]; ok {
		if record.EffortStatus == nil {
			record.EffortStatus = make(map[chat.Participant]chat.EffortStatus)
		}
		record.EffortStatus[participant] = status
		o.room.Workflows[spec.workflowID] = record
	}
}
