package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	cmd    *exec.Cmd
}

type streamEvent struct {
	Event           string     `json:"event"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	Model           string     `json:"model,omitempty"`
	Effort          string     `json:"effort,omitempty"`
	ReasoningEffort string     `json:"reasoning_effort,omitempty"`
	StepUpdate      stepUpdate `json:"step_update,omitempty"`
	Result          result     `json:"result,omitempty"`
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
	ConversationID  string `json:"conversation_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Response        string `json:"response,omitempty"`
	Error           string `json:"error,omitempty"`
	Model           string `json:"model,omitempty"`
	Effort          string `json:"effort,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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

func (c *Client) ProcessAlive() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return false, "AGY provider process is not running"
	}
	return true, "AGY provider process is alive"
}

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
	return c.run(ctx, request, emit, true)
}

func (c *Client) run(ctx context.Context, request agent.TurnRequest, emit func(agent.Event), retryEmptyVoice bool) (agent.TurnResult, error) {
	c.Configure(request.Settings)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return agent.TurnResult{}, fmt.Errorf("AGY client is closed")
	}
	configured := c.config
	c.mu.Unlock()
	if request.Ephemeral || request.VoiceOnly {
		configured.SessionID = ""
	}
	workingDirectory := request.Workspace
	var voiceDirectory string
	if request.VoiceOnly || request.NoTools {
		var err error
		voiceDirectory, err = os.MkdirTemp("", "mohuddle-agy-voice-")
		if err != nil {
			return agent.TurnResult{}, fmt.Errorf("create isolated AGY voice workspace: %w", err)
		}
		defer os.RemoveAll(voiceDirectory)
		workingDirectory = voiceDirectory
	}

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
	if request.VoiceOnly || request.NoTools {
		// AGY CLI 1.1.18 can list workspace custom agents but may fall back to
		// the default agent when --agent selects one in print mode. Run a direct,
		// non-persistent turn instead: slash expansion is disabled, the workspace
		// is empty, the terminal sandbox stays enabled, and any emitted tool step
		// fails the turn below.
		args = append(args, "--disable-slash-commands", "--sandbox")
	} else {
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
	}
	if !request.VoiceOnly && !request.NoTools {
		for _, root := range additionalRoots(request.Workspace, request.ReadRoots) {
			args = append(args, "--add-dir", root)
		}
	}

	cmd := exec.CommandContext(ctx, configured.Binary, args...)
	cmd.Dir = workingDirectory
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
	c.mu.Lock()
	c.cmd = cmd
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.cmd == cmd {
			c.cmd = nil
		}
		c.mu.Unlock()
	}()
	requestPrompt := request.Prompt
	if request.VoiceOnly {
		requestPrompt = compactVoicePrompt(requestPrompt, voicePromptRuneLimit)
	}
	prompt := request.SystemPrompt + "\n\n" + requestPrompt
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
	emit(agent.Event{Type: agent.EventActivity, Agent: chat.Agy, Activity: &agent.ActivityEvent{State: chat.SchedulerActive, Action: "provider call running", Operation: chat.OperationOther, Transition: "provider_call_started"}})
	emit(agent.Event{Type: agent.EventStatus, Agent: chat.Agy, Text: "AGY is working"})

	var collected strings.Builder
	var finalText, resultError, resultStatus string
	var sawResult, voiceToolAttempt bool
	var runtimeModel, runtimeEffort string
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
		if model := strings.TrimSpace(event.Model); model != "" {
			runtimeModel = model
		}
		if model := strings.TrimSpace(event.Result.Model); model != "" {
			runtimeModel = model
		}
		for _, effort := range []string{event.ReasoningEffort, event.Effort, event.Result.ReasoningEffort, event.Result.Effort} {
			if effort = strings.TrimSpace(effort); effort != "" {
				runtimeEffort = effort
				break
			}
		}
		switch event.Event {
		case "step_update":
			if (request.VoiceOnly || request.NoTools) && event.StepUpdate.StepType == "tool" {
				voiceToolAttempt = true
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				continue
			}
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
	if voiceToolAttempt {
		return agent.TurnResult{}, fmt.Errorf("AGY isolated read-only turn attempted to use a tool")
	}
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
	result := agent.ParseTurnResult(finalText, resultSession)
	result.RuntimeModel = runtimeModel
	result.RuntimeEffort = runtimeEffort
	if runtimeModel != "" || runtimeEffort != "" {
		result.RuntimeSource = "agy stream metadata"
	}
	if request.VoiceOnly && request.PublicResponseRequired && strings.TrimSpace(result.Text) == "" {
		if retryEmptyVoice {
			retry := request
			retry.Prompt = emptyVoiceRetryPrompt(request.Prompt)
			emit(agent.Event{Type: agent.EventStatus, Agent: chat.Agy, Text: "AGY returned no public reply; retrying the latest request"})
			return c.run(ctx, retry, emit, false)
		}
		return agent.TurnResult{}, fmt.Errorf("AGY isolated read-only turn returned no public response")
	}
	if !request.Ephemeral && !request.VoiceOnly {
		c.mu.Lock()
		c.config.SessionID = resultSession
		c.mu.Unlock()
	} else {
		result.SessionID = ""
	}
	return result, nil
}

const (
	voicePromptRuneLimit = 24 * 1024
	voiceRetryRuneLimit  = 6 * 1024
)

func compactVoicePrompt(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	tail := string(runes[len(runes)-limit:])
	// Prefer starting at a transcript message boundary rather than in the
	// middle of an older message. The most recent request remains at the end.
	if boundary := strings.Index(tail, "\n["); boundary >= 0 {
		tail = tail[boundary+1:]
	}
	return "Earlier room history was omitted to keep this voice turn focused. The latest transcript and current request follow.\n\n" + tail
}

func emptyVoiceRetryPrompt(value string) string {
	return "Your previous attempt returned only the private control marker and no public speech. This turn explicitly requires a brief, direct answer. Focus on the latest human message in the transcript, answer it in one or two sentences, use no tools, and then include the required private control marker.\n\n" + compactVoicePrompt(value, voiceRetryRuneLimit)
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

func (c *Client) ResetSession() {
	c.mu.Lock()
	c.config.SessionID = ""
	c.mu.Unlock()
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
