package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
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

type streamMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Errors    []string        `json:"errors,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

func New(config Config) *Client {
	if config.Binary == "" {
		config.Binary = "claude"
	}
	if !config.Permissions.Valid() {
		config.Permissions = chat.PermissionWorkspace
	}
	return &Client{config: config}
}

func (c *Client) Participant() chat.Participant { return chat.Claude }

func (c *Client) Models(context.Context) ([]agent.ModelOption, error) {
	return []agent.ModelOption{
		{ID: "default", Name: "Account default", Default: true},
		{ID: "best", Name: "Best available"},
		{ID: "sonnet", Name: "Latest Sonnet"},
		{ID: "opus", Name: "Latest Opus"},
		{ID: "haiku", Name: "Latest Haiku"},
		{ID: "sonnet[1m]", Name: "Sonnet extended context"},
		{ID: "opus[1m]", Name: "Opus extended context"},
		{ID: "opusplan", Name: "Opus planning, Sonnet execution"},
	}, nil
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
		return agent.TurnResult{}, fmt.Errorf("claude client is closed")
	}
	sessionID := c.config.SessionID
	configured := c.config
	c.mu.Unlock()

	settings, err := settingsJSON(request, configured.Permissions)
	if err != nil {
		return agent.TurnResult{}, err
	}
	permissionMode := "acceptEdits"
	if configured.Permissions == chat.PermissionReadOnly {
		permissionMode = "plan"
	} else if configured.Permissions == chat.PermissionFull {
		permissionMode = "bypassPermissions"
	}
	args := []string{
		"-p",
		"--input-format", "text",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", permissionMode,
		"--append-system-prompt", request.SystemPrompt,
		"--settings", string(settings),
	}
	if configured.Model != "" {
		args = append(args, "--model", configured.Model)
	}
	if configured.Effort != "" && configured.Effort != "auto" {
		args = append(args, "--effort", configured.Effort)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	additionalRoots := uniqueRoots(request.Workspace, request.ReadRoots)
	if len(additionalRoots) > 0 {
		args = append(args, "--add-dir")
		args = append(args, additionalRoots...)
	}

	cmd := exec.CommandContext(ctx, configured.Binary, args...)
	cmd.Dir = request.Workspace
	cmd.Stdin = strings.NewReader(request.Prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.TurnResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return agent.TurnResult{}, fmt.Errorf("start claude: %w", err)
	}
	emit(agent.Event{Type: agent.EventStatus, Agent: chat.Claude, Text: "Claude is working"})

	var collected strings.Builder
	var finalText string
	resultSession := sessionID
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var message streamMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			emit(agent.Event{Type: agent.EventStatus, Agent: chat.Claude, Text: "ignored malformed Claude stream event"})
			continue
		}
		if message.SessionID != "" {
			resultSession = message.SessionID
		}
		switch message.Type {
		case "assistant":
			text, tools := parseAssistant(message.Message)
			if text != "" {
				if collected.Len() > 0 {
					collected.WriteByte('\n')
				}
				collected.WriteString(text)
				emit(agent.Event{Type: agent.EventDelta, Agent: chat.Claude, Text: text})
			}
			for _, tool := range tools {
				emit(agent.Event{Type: agent.EventTool, Agent: chat.Claude, Text: tool})
			}
		case "result":
			if message.Result != "" {
				finalText = message.Result
			}
			if message.IsError {
				return agent.TurnResult{}, fmt.Errorf("claude turn failed: %s", strings.Join(message.Errors, "; "))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return agent.TurnResult{}, fmt.Errorf("read claude stream: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{}, fmt.Errorf("claude exited: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if finalText == "" {
		finalText = collected.String()
	}
	public, control, accessRequest := agent.ParseResponse(finalText)
	c.mu.Lock()
	c.config.SessionID = resultSession
	c.mu.Unlock()
	return agent.TurnResult{Text: public, Done: control.Done, Disagrees: control.Position == "disagree", ConflictReason: control.Reason, AccessRequest: accessRequest, SessionID: resultSession}, nil
}

func settingsJSON(request agent.TurnRequest, profiles ...chat.PermissionProfile) ([]byte, error) {
	profile := chat.PermissionWorkspace
	if len(profiles) > 0 && profiles[0].Valid() {
		profile = profiles[0]
	}
	if profile == chat.PermissionFull {
		return json.Marshal(map[string]any{
			"permissions": map[string]any{"defaultMode": "bypassPermissions"},
			"sandbox":     map[string]any{"enabled": false},
		})
	}
	readOnlyRoots := difference(request.ReadRoots, request.WriteRoots)
	writeRoots := request.WriteRoots
	defaultMode := "acceptEdits"
	if profile == chat.PermissionReadOnly {
		defaultMode = "plan"
		readOnlyRoots = request.ReadRoots
		writeRoots = []string{}
	}
	editDenyRules := make([]string, 0, len(readOnlyRoots))
	for _, root := range readOnlyRoots {
		// Claude permission rules use // for absolute paths. denyWrite below
		// independently applies the same boundary to sandboxed subprocesses.
		editDenyRules = append(editDenyRules, "Edit(//"+strings.TrimPrefix(filepath.ToSlash(root), "/")+"/**)")
	}
	return json.Marshal(map[string]any{
		"permissions": map[string]any{
			"defaultMode":           defaultMode,
			"additionalDirectories": request.ReadRoots,
			"deny":                  editDenyRules,
		},
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			"filesystem": map[string]any{
				"allowRead":  request.ReadRoots,
				"allowWrite": writeRoots,
				"denyWrite":  readOnlyRoots,
			},
			"network": map[string]any{
				"allowedDomains": []string{},
			},
		},
	})
}

func parseAssistant(raw json.RawMessage) (string, []string) {
	var message assistantMessage
	if json.Unmarshal(raw, &message) != nil {
		return "", nil
	}
	var text []string
	var tools []string
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				text = append(text, block.Text)
			}
		case "tool_use":
			detail := block.Name
			for _, key := range []string{"command", "file_path", "path", "query"} {
				if value, ok := block.Input[key]; ok {
					detail += ": " + fmt.Sprint(value)
					break
				}
			}
			if detail != "" {
				tools = append(tools, detail)
			}
		}
	}
	return strings.Join(text, "\n"), tools
}

func uniqueRoots(workspace string, roots []string) []string {
	seen := map[string]bool{workspace: true}
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		result = append(result, root)
	}
	slices.Sort(result)
	return result
}

func difference(values, excluded []string) []string {
	excludedSet := make(map[string]bool, len(excluded))
	for _, value := range excluded {
		excludedSet[value] = true
	}
	var result []string
	for _, value := range values {
		if value != "" && !excludedSet[value] {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
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
