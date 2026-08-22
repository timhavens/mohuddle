package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

type Config struct {
	Binary      string
	Model       string
	Effort      string
	Permissions chat.PermissionProfile
	SessionID   string
}

type Client struct {
	config Config
	mu     sync.Mutex
	closed bool
}

type streamEvent struct {
	Event          string     `json:"event"`
	ConversationID string     `json:"conversation_id,omitempty"`
	StepUpdate     stepUpdate `json:"step_update,omitempty"`
	Result         result     `json:"result,omitempty"`
}

type stepUpdate struct {
	ConversationID string         `json:"conversation_id,omitempty"`
	State          string         `json:"state,omitempty"`
	StepType       string         `json:"step_type,omitempty"`
	ToolName       string         `json:"tool_name,omitempty"`
	TextDelta      string         `json:"text_delta,omitempty"`
	ToolInfo       map[string]any `json:"tool_info,omitempty"`
}

type result struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Status         string `json:"status,omitempty"`
	Response       string `json:"response,omitempty"`
	Error          string `json:"error,omitempty"`
}

func New(config Config) *Client {
	if config.Binary == "" {
		config.Binary = "agy"
	}
	if !config.Permissions.Valid() {
		config.Permissions = chat.PermissionWorkspace
	}
	return &Client{config: config}
}

func (c *Client) Participant() chat.Participant { return chat.Agy }

func (c *Client) Models(ctx context.Context) ([]agent.ModelOption, error) {
	c.mu.Lock()
	binary := c.config.Binary
	c.mu.Unlock()
	cmd := exec.CommandContext(ctx, binary, "models")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list AGY models: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var models []agent.ModelOption
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(line, "Available ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		models = append(models, agent.ModelOption{ID: fields[0], Name: name, Efforts: []string{"low", "medium", "high"}})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("AGY returned no models")
	}
	return models, nil
}

func (c *Client) Configure(value chat.AgentSettings) bool {
	value = value.WithDefaults()
	c.mu.Lock()
	defer c.mu.Unlock()
	reset := c.config.SessionID != "" && (c.config.Model != value.Model || c.config.Effort != value.Effort)
	c.config.Model = value.Model
	c.config.Effort = value.Effort
	c.config.Permissions = value.Permissions
	if reset {
		c.config.SessionID = ""
	}
	return reset
}

func (c *Client) Run(ctx context.Context, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	c.Configure(request.Settings)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return agent.TurnResult{}, fmt.Errorf("AGY client is closed")
	}
	configured := c.config
	c.mu.Unlock()

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--print-timeout", "15m",
	}
	if configured.SessionID != "" {
		args = append(args, "--conversation", configured.SessionID)
	}
	if configured.Model != "" {
		args = append(args, "--model", configured.Model)
	}
	if configured.Effort != "" && configured.Effort != "auto" {
		args = append(args, "--effort", configured.Effort)
	}
	switch configured.Permissions {
	case chat.PermissionReadOnly:
		// AGY implements plan mode by prepending /plan, so disabling slash
		// expansion makes the mode ineffective. Headless mode cannot display
		// permission prompts; auto-approve the read-only tools selected by plan
		// mode while retaining AGY's native terminal sandbox.
		args = append(args, "--mode", "plan", "--sandbox", "--dangerously-skip-permissions")
	case chat.PermissionFull:
		args = append(args, "--disable-slash-commands", "--mode", "accept-edits", "--dangerously-skip-permissions")
	default:
		args = append(args, "--disable-slash-commands", "--mode", "accept-edits", "--sandbox", "--dangerously-skip-permissions")
	}
	for _, root := range additionalRoots(request.Workspace, request.ReadRoots) {
		args = append(args, "--add-dir", root)
	}

	cmd := exec.CommandContext(ctx, configured.Binary, args...)
	cmd.Dir = request.Workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return agent.TurnResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.TurnResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return agent.TurnResult{}, fmt.Errorf("start AGY: %w", err)
	}
	prompt := request.SystemPrompt + "\n\n" + request.Prompt
	input := map[string]any{"event": "user", "message": map[string]string{"content": prompt}}
	if err := json.NewEncoder(stdin).Encode(input); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return agent.TurnResult{}, fmt.Errorf("send AGY prompt: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		return agent.TurnResult{}, fmt.Errorf("close AGY prompt stream: %w", err)
	}
	emit(agent.Event{Type: agent.EventStatus, Agent: chat.Agy, Text: "AGY is working"})

	var collected strings.Builder
	var finalText, resultError, resultStatus string
	var sawResult bool
	resultSession := configured.SessionID
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			emit(agent.Event{Type: agent.EventStatus, Agent: chat.Agy, Text: "ignored malformed AGY stream event"})
			continue
		}
		for _, id := range []string{event.ConversationID, event.StepUpdate.ConversationID, event.Result.ConversationID} {
			if id != "" {
				resultSession = id
			}
		}
		switch event.Event {
		case "step_update":
			if event.StepUpdate.TextDelta != "" {
				collected.WriteString(event.StepUpdate.TextDelta)
				emit(agent.Event{Type: agent.EventDelta, Agent: chat.Agy, Text: event.StepUpdate.TextDelta})
			}
			if event.StepUpdate.StepType == "tool" && event.StepUpdate.State == "DONE" {
				emit(agent.Event{Type: agent.EventTool, Agent: chat.Agy, Text: agyToolSummary(event.StepUpdate)})
			}
		case "result":
			sawResult = true
			resultStatus = strings.TrimSpace(event.Result.Status)
			finalText = event.Result.Response
			if event.Result.Status != "" && event.Result.Status != "SUCCESS" {
				resultError = strings.TrimSpace(event.Result.Error)
				if resultError == "" {
					resultError = "status " + event.Result.Status
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return agent.TurnResult{}, fmt.Errorf("read AGY stream: %w", err)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return agent.TurnResult{}, ctx.Err()
	}
	if waitErr == nil && !sawResult {
		return agent.TurnResult{}, fmt.Errorf("AGY turn ended without a result event: %s", strings.TrimSpace(stderr.String()))
	}
	if waitErr != nil || resultError != "" {
		detail := strings.Trim(strings.TrimSpace(resultError)+": "+strings.TrimSpace(stderr.String()), ": ")
		if agyCancellation(resultStatus, detail) {
			return agent.TurnResult{}, fmt.Errorf("AGY turn canceled: %w", context.Canceled)
		}
		if waitErr != nil {
			return agent.TurnResult{}, fmt.Errorf("AGY turn failed: %w: %s", waitErr, detail)
		}
		return agent.TurnResult{}, fmt.Errorf("AGY turn failed: %s", detail)
	}
	if finalText == "" {
		finalText = collected.String()
	}
	public, control, accessRequest := agent.ParseResponse(finalText)
	c.mu.Lock()
	c.config.SessionID = resultSession
	c.mu.Unlock()
	return agent.TurnResult{
		Text: public, SessionID: resultSession, Done: control.Done,
		Disagrees: control.Position == "disagree", ConflictReason: control.Reason,
		AccessRequest: accessRequest,
	}, nil
}

func agyCancellation(status, detail string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "CANCELED", "CANCELLED":
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(detail))
	return strings.Contains(normalized, "context canceled") || strings.Contains(normalized, "context cancelled")
}

func additionalRoots(workspace string, roots []string) []string {
	seen := map[string]bool{workspace: true}
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != "" && !seen[root] {
			seen[root] = true
			result = append(result, root)
		}
	}
	slices.Sort(result)
	return result
}

func agyToolSummary(update stepUpdate) string {
	detail := update.ToolName
	if detail == "" {
		detail = "tool"
	}
	if parameters, ok := update.ToolInfo["parameters"].(map[string]any); ok {
		for _, key := range []string{"CommandLine", "command", "file_path", "path", "query"} {
			if value, found := parameters[key]; found {
				return detail + ": " + fmt.Sprint(value)
			}
		}
	}
	return detail
}

func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.SessionID
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
