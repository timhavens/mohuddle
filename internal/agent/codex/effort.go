package codex

import (
	"context"
	"fmt"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

// turn/start effort overrides persist in the native thread. Sending an explicit
// provider default is necessary when returning from an override to auto/omitted.
// A new thread's settings resolve the baseline without a model call. Resumed
// threads and changed models resolve it from config/read and model/list instead
// of mistaking the last persisted override for the provider default.
func (c *Client) turnEffort(ctx context.Context, configured Config, workspace string) (string, error) {
	if configured.Effort != "" && configured.Effort != "auto" {
		return configured.Effort, nil
	}
	c.mu.Lock()
	baseline := c.defaultEffort
	needsLookup := c.effortOverride || c.defaultNeedsLookup
	c.mu.Unlock()
	if baseline != "" {
		return baseline, nil
	}
	if !needsLookup {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTurnStartTimeout)
	defer cancel()
	var configuration struct {
		Config struct {
			Model  string `json:"model"`
			Effort string `json:"model_reasoning_effort"`
		} `json:"config"`
	}
	if err := c.call(ctx, "config/read", map[string]any{"cwd": workspace, "includeLayers": false}, &configuration); err != nil {
		return "", &chat.EffortError{Message: "could not resolve Codex provider-default effort; no turn was started"}
	}
	baseline = configuration.Config.Effort
	model := configured.Model
	if model == "" {
		model = configuration.Config.Model
	}
	params := map[string]any{}
	for page := 0; baseline == "" && page < 100; page++ {
		var catalog struct {
			Data []struct {
				ID            string `json:"id"`
				Model         string `json:"model"`
				Default       bool   `json:"isDefault"`
				DefaultEffort string `json:"defaultReasoningEffort"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "model/list", params, &catalog); err != nil {
			break
		}
		for _, option := range catalog.Data {
			if (model == "" && option.Default) || (model != "" && (option.Model == model || option.ID == model)) {
				baseline = option.DefaultEffort
				break
			}
		}
		if catalog.NextCursor == "" || params["cursor"] == catalog.NextCursor {
			break
		}
		params["cursor"] = catalog.NextCursor
	}
	if strings.TrimSpace(baseline) == "" {
		return "", &chat.EffortError{Message: fmt.Sprintf("Codex did not report a provider-default effort for model %q; no turn was started", model)}
	}
	c.mu.Lock()
	c.defaultEffort = baseline
	c.mu.Unlock()
	return baseline, nil
}
