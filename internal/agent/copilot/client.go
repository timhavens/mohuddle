package copilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

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

type accessPolicy struct {
	profile    chat.PermissionProfile
	workspace  string
	readRoots  []string
	writeRoots []string
}

type Client struct {
	config Config

	mu      sync.Mutex
	client  *sdk.Client
	session *sdk.Session
	policy  accessPolicy
	closed  bool
}

func New(config Config) *Client {
	if config.Binary == "" {
		config.Binary = "copilot"
	}
	if !config.Permissions.Valid() {
		config.Permissions = chat.PermissionWorkspace
	}
	return &Client{config: config}
}

func (c *Client) Participant() chat.Participant { return chat.Copilot }

func (c *Client) Models(ctx context.Context) ([]agent.ModelOption, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Copilot models: %w", err)
	}
	result := make([]agent.ModelOption, 0, len(models))
	for _, model := range models {
		result = append(result, agent.ModelOption{
			ID: model.ID, Name: model.Name, Efforts: append([]string(nil), model.SupportedReasoningEfforts...),
		})
	}
	return result, nil
}

func (c *Client) Configure(value chat.AgentSettings) bool {
	value = value.WithDefaults()
	c.mu.Lock()
	reset := c.config.SessionID != "" && (c.config.Model != value.Model || c.config.Effort != value.Effort)
	c.config.Model = value.Model
	c.config.Effort = value.Effort
	c.config.Permissions = value.Permissions
	var session *sdk.Session
	if reset {
		session = c.session
		c.session = nil
		c.config.SessionID = ""
	}
	c.mu.Unlock()
	if session != nil {
		_ = session.Disconnect()
	}
	return reset
}

func (c *Client) Run(ctx context.Context, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	c.Configure(request.Settings)
	if err := c.ensureStarted(ctx); err != nil {
		return agent.TurnResult{}, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return agent.TurnResult{}, fmt.Errorf("Copilot client is closed")
	}
	configured := c.config
	transient := request.Ephemeral || request.VoiceOnly || request.NoTools
	if transient {
		configured.SessionID = ""
	}
	c.policy = accessPolicy{
		profile: configured.Permissions, workspace: request.Workspace,
		readRoots: append([]string(nil), request.ReadRoots...), writeRoots: append([]string(nil), request.WriteRoots...),
	}
	client := c.client
	previousSession := c.session
	c.session = nil
	c.mu.Unlock()
	if previousSession != nil {
		_ = previousSession.Disconnect()
	}

	session, err := c.openSession(ctx, client, configured, request)
	if err != nil {
		return agent.TurnResult{}, err
	}
	if transient {
		defer session.Disconnect()
	} else {
		c.mu.Lock()
		c.session = session
		c.config.SessionID = session.SessionID
		c.mu.Unlock()
	}

	emit(agent.Event{Type: agent.EventStatus, Agent: chat.Copilot, Text: "Copilot is working"})
	done := make(chan struct{}, 1)
	errors := make(chan error, 1)
	var resultMu sync.Mutex
	var collected, final strings.Builder
	unsubscribe := session.On(func(event sdk.SessionEvent) {
		if event.AgentID != nil {
			return
		}
		switch data := event.Data.(type) {
		case *sdk.AssistantMessageDeltaData:
			resultMu.Lock()
			collected.WriteString(data.DeltaContent)
			resultMu.Unlock()
			emit(agent.Event{Type: agent.EventDelta, Agent: chat.Copilot, Text: data.DeltaContent})
		case *sdk.AssistantMessageData:
			resultMu.Lock()
			if final.Len() > 0 {
				final.WriteByte('\n')
			}
			final.WriteString(data.Content)
			resultMu.Unlock()
		case *sdk.ToolExecutionStartData:
			if transient {
				select {
				case errors <- fmt.Errorf("Copilot attempted tool use during a no-tools turn: %s", copilotToolSummary(data)):
				default:
				}
				return
			}
			emit(agent.Event{Type: agent.EventTool, Agent: chat.Copilot, Text: copilotToolSummary(data)})
		case *sdk.SessionErrorData:
			select {
			case errors <- copilotSessionError(data):
			default:
			}
		case *sdk.SessionIdleData:
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})
	defer unsubscribe()
	options := sdk.MessageOptions{Prompt: request.Prompt}
	options.Attachments = copilotAttachments(request.Attachments)
	if _, err := session.Send(ctx, options); err != nil {
		return agent.TurnResult{}, fmt.Errorf("send Copilot prompt: %w", err)
	}
	select {
	case <-ctx.Done():
		_ = session.Abort(context.Background())
		return agent.TurnResult{}, ctx.Err()
	case err := <-errors:
		_ = session.Abort(context.Background())
		return agent.TurnResult{}, fmt.Errorf("Copilot turn failed: %w", err)
	case <-done:
	}

	resultMu.Lock()
	text := final.String()
	if strings.TrimSpace(text) == "" {
		text = collected.String()
	}
	resultMu.Unlock()
	result := agent.ParseTurnResult(text, session.SessionID)
	if transient {
		result.SessionID = ""
	}
	return result, nil
}

func copilotSessionError(data *sdk.SessionErrorData) error {
	code := ""
	if data.ErrorCode != nil {
		code = strings.TrimSpace(*data.ErrorCode)
	}
	confirmedParticipantLimit := (data.ErrorType == "quota" && (code == "quota_exceeded" || code == "session_quota_exceeded")) ||
		(data.ErrorType == "rate_limit" && (code == "user_weekly_rate_limited" || code == "user_global_rate_limited"))
	if !confirmedParticipantLimit {
		return fmt.Errorf("%s", data.Message)
	}
	reason := strings.TrimSpace(data.Message)
	if code != "" {
		reason = code + ": " + reason
	}
	return &agent.AvailabilityError{
		Participant: chat.Copilot, Reason: reason, Source: "copilot-sdk", Confidence: "confirmed",
	}
}

func copilotAttachments(attachments []chat.Attachment) []sdk.Attachment {
	var result []sdk.Attachment
	for _, attachment := range attachments {
		if attachment.Kind != chat.AttachmentImage || strings.TrimSpace(attachment.Path) == "" {
			continue
		}
		result = append(result, sdk.AttachmentFile{DisplayName: attachment.Name, Path: attachment.Path})
	}
	return result
}

func (c *Client) ensureStarted(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("Copilot client is closed")
	}
	if c.client != nil {
		return nil
	}
	home := strings.TrimSpace(os.Getenv("COPILOT_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(userHome, ".copilot")
	}
	client := sdk.NewClient(&sdk.ClientOptions{
		Connection:    sdk.StdioConnection{Path: c.config.Binary},
		BaseDirectory: home,
		LogLevel:      "error",
		Mode:          sdk.ModeEmpty,
	})
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("start Copilot SDK runtime: %w", err)
	}
	c.client = client
	return nil
}

func (c *Client) openSession(ctx context.Context, client *sdk.Client, configured Config, request agent.TurnRequest) (*sdk.Session, error) {
	model := configured.Model
	if model == "" {
		model = "auto"
	}
	tools := copilotTools(configured.Permissions, request.VoiceOnly || request.NoTools)
	persist := !(request.Ephemeral || request.VoiceOnly || request.NoTools)
	additional := additionalDirectories(configured.Permissions, request)
	system := &sdk.SystemMessageConfig{Content: request.SystemPrompt}
	if configured.SessionID == "" {
		session, err := client.CreateSession(ctx, &sdk.SessionConfig{
			ClientName: "mohuddle", Model: model, ReasoningEffort: configured.Effort,
			SystemMessage: system, AvailableTools: tools, OnPermissionRequest: c.permissionDecision,
			WorkingDirectory: request.Workspace, AdditionalDirectories: additional, Streaming: sdk.Bool(true),
			EnableConfigDiscovery: sdk.Bool(false), EnableSkills: sdk.Bool(false), EnableSessionStore: sdk.Bool(persist),
			SkipCustomInstructions: sdk.Bool(true), IncludeSubAgentStreamingEvents: sdk.Bool(false),
		})
		if err != nil {
			return nil, fmt.Errorf("create Copilot session: %w", err)
		}
		return session, nil
	}
	session, err := client.ResumeSession(ctx, configured.SessionID, &sdk.ResumeSessionConfig{
		ClientName: "mohuddle", Model: model, ReasoningEffort: configured.Effort,
		SystemMessage: system, AvailableTools: tools, OnPermissionRequest: c.permissionDecision,
		WorkingDirectory: request.Workspace, AdditionalDirectories: additional, Streaming: sdk.Bool(true),
		EnableConfigDiscovery: sdk.Bool(false), EnableSkills: sdk.Bool(false), EnableSessionStore: sdk.Bool(persist),
		SkipCustomInstructions: sdk.Bool(true), IncludeSubAgentStreamingEvents: sdk.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("resume Copilot session: %w", err)
	}
	return session, nil
}

func copilotTools(profile chat.PermissionProfile, voiceOnly ...bool) []string {
	if len(voiceOnly) > 0 && voiceOnly[0] {
		// A non-nil empty allowlist is significant in SDK ModeEmpty: it exposes
		// no built-in or custom tools to the model.
		return make([]string, 0)
	}
	tools := sdk.NewToolSet()
	switch profile {
	case chat.PermissionReadOnly:
		tools.AddBuiltIn("view", "grep")
	case chat.PermissionFull:
		tools.AddBuiltIn("*")
	default:
		tools.AddBuiltIn("view", "grep", "edit", "bash", "powershell")
	}
	for _, name := range sdk.BuiltInToolsIsolated {
		if name != "ask_user" {
			tools.AddBuiltIn(name)
		}
	}
	return tools.ToSlice()
}

func additionalDirectories(profile chat.PermissionProfile, request agent.TurnRequest) []string {
	if profile == chat.PermissionFull {
		volume := filepath.VolumeName(request.Workspace)
		return []string{volume + string(filepath.Separator)}
	}
	seen := map[string]bool{request.Workspace: true}
	var result []string
	for _, root := range request.ReadRoots {
		if root != "" && !seen[root] {
			seen[root] = true
			result = append(result, root)
		}
	}
	return result
}

func (c *Client) permissionDecision(request sdk.PermissionRequest, invocation sdk.PermissionInvocation) (rpc.PermissionDecision, error) {
	c.mu.Lock()
	policy := c.policy
	c.mu.Unlock()
	if invocation.ManagedSettingsEnabled || request.RequiresManagedApproval() {
		return rejectPermission("managed policy requires an explicit approval"), nil
	}
	if policy.profile == chat.PermissionFull {
		return &rpc.PermissionDecisionApproveOnce{}, nil
	}
	switch value := request.(type) {
	case rpc.PermissionRequestRead:
		if pathWithinAny(value.Path, policy.workspace, policy.readRoots) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	case *rpc.PermissionRequestRead:
		if pathWithinAny(value.Path, policy.workspace, policy.readRoots) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	case rpc.PermissionRequestWrite:
		if policy.profile == chat.PermissionWorkspace && pathWithinAny(value.FileName, policy.workspace, policy.writeRoots) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	case *rpc.PermissionRequestWrite:
		if policy.profile == chat.PermissionWorkspace && pathWithinAny(value.FileName, policy.workspace, policy.writeRoots) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	case rpc.PermissionRequestShell:
		if policy.profile == chat.PermissionWorkspace && shellAllowed(value, policy) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	case *rpc.PermissionRequestShell:
		if policy.profile == chat.PermissionWorkspace && shellAllowed(*value, policy) {
			return &rpc.PermissionDecisionApproveOnce{}, nil
		}
	}
	return rejectPermission("blocked by the MoHuddle permission profile"), nil
}

func shellAllowed(request rpc.PermissionRequestShell, policy accessPolicy) bool {
	if request.RequestSandboxBypass != nil && *request.RequestSandboxBypass {
		return false
	}
	if len(request.PossibleURLs) > 0 {
		return false
	}
	networkCommands := map[string]bool{
		"curl": true, "wget": true, "ssh": true, "scp": true, "sftp": true,
		"nc": true, "ncat": true, "netcat": true, "telnet": true,
	}
	mutating := request.HasWriteFileRedirection
	for _, command := range request.Commands {
		name := strings.ToLower(filepath.Base(command.Identifier))
		if networkCommands[name] {
			return false
		}
		if !command.ReadOnly {
			mutating = true
		}
	}
	lower := strings.ToLower(request.FullCommandText)
	for _, token := range []string{"curl ", "wget ", "ssh ", "scp ", "sftp ", "nc ", "ncat ", "netcat ", "telnet "} {
		if strings.Contains(lower, token) {
			return false
		}
	}
	for _, path := range request.PossiblePaths {
		roots := policy.readRoots
		if mutating {
			roots = policy.writeRoots
		}
		if !pathWithinAny(path, policy.workspace, roots) {
			return false
		}
	}
	return true
}

func pathWithinAny(path, workspace string, roots []string) bool {
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	for _, root := range roots {
		root = filepath.Clean(root)
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func rejectPermission(message string) rpc.PermissionDecision {
	return &rpc.PermissionDecisionReject{Feedback: &message}
}

func copilotToolSummary(data *sdk.ToolExecutionStartData) string {
	detail := data.ToolName
	if detail == "" {
		detail = "tool"
	}
	if data.Arguments != nil {
		text := strings.Join(strings.Fields(fmt.Sprint(data.Arguments)), " ")
		if len([]rune(text)) > 160 {
			text = string([]rune(text)[:159]) + "…"
		}
		if text != "" {
			detail += ": " + text
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
	session := c.session
	c.session = nil
	c.config.SessionID = ""
	c.mu.Unlock()
	if session != nil {
		_ = session.Disconnect()
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	session := c.session
	client := c.client
	c.session = nil
	c.client = nil
	c.mu.Unlock()
	if session != nil {
		_ = session.Disconnect()
	}
	if client != nil {
		return client.Stop()
	}
	return nil
}
