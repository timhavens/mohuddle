package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	mu               sync.Mutex
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	threadID         string
	workspace        string
	started          bool
	closed           bool
	turnStartTimeout time.Duration

	writeMu sync.Mutex
	nextID  atomic.Int64
	pending sync.Map
	events  chan rpcMessage
	errCh   chan error
	waitCh  chan error
	stderr  lockedBuffer
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type callResult struct {
	result json.RawMessage
	err    error
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

const defaultTurnStartTimeout = 15 * time.Second

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len()+len(value) > 128*1024 {
		current := b.b.Bytes()
		keep := 64 * 1024
		if len(current) > keep {
			current = current[len(current)-keep:]
		}
		b.b.Reset()
		_, _ = b.b.Write(current)
	}
	return b.b.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func New(config Config) *Client {
	if config.Binary == "" {
		config.Binary = "codex"
	}
	if !config.Permissions.Valid() {
		config.Permissions = chat.PermissionWorkspace
	}
	return &Client{
		config: config, turnStartTimeout: defaultTurnStartTimeout,
		events: make(chan rpcMessage, 256), errCh: make(chan error, 1), waitCh: make(chan error, 1),
	}
}

func (c *Client) Participant() chat.Participant { return chat.Codex }

func (c *Client) Models(ctx context.Context) ([]agent.ModelOption, error) {
	c.mu.Lock()
	binary := c.config.Binary
	c.mu.Unlock()
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex model catalog: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	write := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(data, '\n'))
		return err
	}
	if err := write(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]any{"name": "mohuddle", "title": "MoHuddle", "version": "0.1.0"},
	}}); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	readResponse := func(id string, target any) error {
		for scanner.Scan() {
			var message rpcMessage
			if json.Unmarshal(scanner.Bytes(), &message) != nil || string(message.ID) != id {
				continue
			}
			if message.Error != nil {
				return fmt.Errorf("Codex RPC error %d: %s", message.Error.Code, message.Error.Message)
			}
			return json.Unmarshal(message.Result, target)
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("Codex model catalog exited: %s", strings.TrimSpace(stderr.String()))
	}
	var initialized map[string]any
	if err := readResponse("1", &initialized); err != nil {
		return nil, err
	}
	if err := write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err := write(map[string]any{"id": 2, "method": "model/list", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID        string `json:"id"`
			Model     string `json:"model"`
			Name      string `json:"displayName"`
			IsDefault bool   `json:"isDefault"`
			Efforts   []struct {
				Value string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
	}
	if err := readResponse("2", &response); err != nil {
		return nil, err
	}
	models := make([]agent.ModelOption, 0, len(response.Data))
	for _, item := range response.Data {
		id := item.Model
		if id == "" {
			id = item.ID
		}
		option := agent.ModelOption{ID: id, Name: item.Name, Default: item.IsDefault}
		for _, effort := range item.Efforts {
			option.Efforts = append(option.Efforts, effort.Value)
		}
		models = append(models, option)
	}
	return models, nil
}

func (c *Client) Configure(value chat.AgentSettings) bool {
	value = value.WithDefaults()
	c.mu.Lock()
	c.config.Model = value.Model
	c.config.Effort = value.Effort
	c.config.Permissions = value.Permissions
	c.mu.Unlock()
	// Codex supports model, effort, approval, and sandbox overrides on turn/start,
	// so its existing thread can continue without replaying the transcript.
	return false
}

func (c *Client) Run(ctx context.Context, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
	c.Configure(request.Settings)
	if request.Ephemeral {
		c.mu.Lock()
		configured := c.config
		c.mu.Unlock()
		configured.SessionID = ""
		temporary := New(configured)
		defer temporary.Close()
		if request.NoTools {
			workspace, err := os.MkdirTemp("", "mohuddle-codex-private-")
			if err != nil {
				return agent.TurnResult{}, fmt.Errorf("create isolated Codex routing workspace: %w", err)
			}
			defer os.RemoveAll(workspace)
			request.Workspace = workspace
			request.ReadRoots = nil
			request.WriteRoots = nil
		}
		request.Ephemeral = false
		result, err := temporary.Run(ctx, request, emit)
		result.SessionID = ""
		return result, err
	}
	input := []map[string]any{{"type": "text", "text": request.Prompt}}
	for _, attachment := range request.Attachments {
		if attachment.Kind == chat.AttachmentImage && strings.TrimSpace(attachment.Path) != "" {
			input = append(input, map[string]any{"type": "localImage", "path": attachment.Path})
		}
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureStarted(ctx, request); err != nil {
			return agent.TurnResult{}, err
		}
		c.mu.Lock()
		configured := c.config
		threadID := c.threadID
		c.mu.Unlock()
		params := map[string]any{
			"threadId":          threadID,
			"input":             input,
			"cwd":               request.Workspace,
			"approvalPolicy":    "never",
			"approvalsReviewer": "user",
			"sandboxPolicy":     sandboxPolicy(configured.Permissions, request.WriteRoots),
		}
		if request.NoTools {
			params["environments"] = []any{}
			params["runtimeWorkspaceRoots"] = []string{}
		}
		if configured.Model != "" {
			params["model"] = configured.Model
		}
		if configured.Effort != "" && configured.Effort != "auto" {
			params["effort"] = configured.Effort
		}
		startCtx, cancelStart := context.WithTimeout(ctx, c.turnStartTimeout)
		err := c.call(startCtx, "turn/start", params, &started)
		internalTimeout := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		cancelStart()
		if err == nil {
			break
		}
		if !internalTimeout {
			return agent.TurnResult{}, err
		}
		c.resetProcess()
		if attempt == 1 {
			return agent.TurnResult{}, fmt.Errorf("codex turn/start timed out after %s", c.turnStartTimeout)
		}
	}
	turnID := started.Turn.ID
	emit(agent.Event{Type: agent.EventStatus, Agent: chat.Codex, Text: "Codex is working"})

	var output strings.Builder
	var completedOutput strings.Builder
	for {
		select {
		case <-ctx.Done():
			c.interrupt(turnID)
			return agent.TurnResult{}, ctx.Err()
		case err := <-c.errCh:
			return agent.TurnResult{}, err
		case message := <-c.events:
			if len(message.ID) > 0 && message.Method != "" {
				if err := c.handleServerRequest(ctx, message, emit); err != nil {
					return agent.TurnResult{}, err
				}
				continue
			}
			switch message.Method {
			case "item/agentMessage/delta":
				var params struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(message.Params, &params) == nil && params.Delta != "" {
					output.WriteString(params.Delta)
					emit(agent.Event{Type: agent.EventDelta, Agent: chat.Codex, Text: params.Delta})
				}
			case "item/started", "item/completed":
				if message.Method == "item/completed" {
					if text, messageTurnID := completedAgentMessage(message.Params); text != "" && (messageTurnID == "" || messageTurnID == turnID) {
						completedOutput.Reset()
						completedOutput.WriteString(text)
					}
				}
				if summary := summarizeItem(message.Params); summary != "" {
					if request.NoTools {
						c.interrupt(turnID)
						return agent.TurnResult{}, fmt.Errorf("Codex attempted tool use during a no-tools turn: %s", summary)
					}
					emit(agent.Event{Type: agent.EventTool, Agent: chat.Codex, Text: summary})
				}
			case "turn/completed":
				var params struct {
					Turn struct {
						ID     string `json:"id"`
						Status string `json:"status"`
						Error  any    `json:"error"`
					} `json:"turn"`
				}
				if json.Unmarshal(message.Params, &params) != nil || (turnID != "" && params.Turn.ID != turnID) {
					continue
				}
				if params.Turn.Status == "failed" {
					return agent.TurnResult{}, fmt.Errorf("codex turn failed: %v", params.Turn.Error)
				}
				c.mu.Lock()
				c.config.SessionID = c.threadID
				c.mu.Unlock()
				finalOutput := output.String()
				if completedOutput.Len() > 0 {
					finalOutput = completedOutput.String()
				}
				return agent.ParseTurnResult(finalOutput, c.threadID), nil
			case "error":
				var params struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if params.Message != "" {
					emit(agent.Event{Type: agent.EventStatus, Agent: chat.Codex, Text: params.Message})
				}
			}
		}
	}
}

// completedAgentMessage extracts the authoritative text carried by an
// item/completed notification. The completed item is the protocol's canonical
// final answer; using it prevents streamed rendering details from changing
// orchestration metadata parsing.
func completedAgentMessage(raw json.RawMessage) (text, turnID string) {
	var params struct {
		TurnID string `json:"turnId"`
		Item   struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil || params.Item.Type != "agentMessage" || (params.Item.Phase != "" && params.Item.Phase != "final_answer") {
		return "", params.TurnID
	}
	return params.Item.Text, params.TurnID
}

func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threadID
}

func (c *Client) ResetSession() {
	c.mu.Lock()
	started := c.started
	c.config.SessionID = ""
	c.threadID = ""
	c.mu.Unlock()
	if started {
		c.resetProcess()
	}
}

func (c *Client) ensureStarted(ctx context.Context, request agent.TurnRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("codex client is closed")
	}
	if c.started {
		return nil
	}
	c.cmd = exec.Command(c.config.Binary, "app-server", "--listen", "stdio://")
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	c.cmd.Stderr = &c.stderr
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}
	c.stdin = stdin
	go c.readLoop(stdout)
	go func() { c.waitCh <- c.cmd.Wait() }()

	var initialized map[string]any
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "mohuddle", "title": "MoHuddle", "version": "0.1.0"},
	}, &initialized); err != nil {
		c.stopProcess()
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.stopProcess()
		return err
	}
	threadParams := map[string]any{
		"cwd":                   request.Workspace,
		"approvalPolicy":        "never",
		"approvalsReviewer":     "user",
		"sandbox":               sandboxMode(c.config.Permissions),
		"developerInstructions": request.SystemPrompt,
	}
	if request.NoTools {
		threadParams["dynamicTools"] = []any{}
		threadParams["environments"] = []any{}
		threadParams["runtimeWorkspaceRoots"] = []string{}
	}
	if c.config.Model != "" {
		threadParams["model"] = c.config.Model
	}
	method := "thread/start"
	if c.config.SessionID != "" {
		method = "thread/resume"
		threadParams["threadId"] = c.config.SessionID
	}
	var threadResult struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.call(ctx, method, threadParams, &threadResult); err != nil {
		if method != "thread/resume" {
			c.stopProcess()
			return fmt.Errorf("%s: %w", method, err)
		}
		delete(threadParams, "threadId")
		if err := c.call(ctx, "thread/start", threadParams, &threadResult); err != nil {
			c.stopProcess()
			return fmt.Errorf("resume and replacement thread start failed: %w", err)
		}
	}
	if threadResult.Thread.ID == "" {
		c.stopProcess()
		return fmt.Errorf("codex returned an empty thread id")
	}
	c.threadID = threadResult.Thread.ID
	c.workspace = request.Workspace
	c.started = true
	return nil
}

func sandboxMode(profile chat.PermissionProfile) string {
	switch profile {
	case chat.PermissionReadOnly:
		return "read-only"
	case chat.PermissionFull:
		return "danger-full-access"
	default:
		return "workspace-write"
	}
}

func sandboxPolicy(profile chat.PermissionProfile, writeRoots []string) map[string]any {
	switch profile {
	case chat.PermissionReadOnly:
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case chat.PermissionFull:
		return map[string]any{"type": "dangerFullAccess"}
	default:
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": writeRoots,
			"networkAccess": false,
		}
	}
}

func (c *Client) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.reportError(fmt.Errorf("decode codex app-server message: %w", err))
			continue
		}
		if len(message.ID) > 0 && message.Method == "" {
			key := string(message.ID)
			if value, ok := c.pending.LoadAndDelete(key); ok {
				result := callResult{result: message.Result}
				if message.Error != nil {
					result.err = fmt.Errorf("codex RPC error %d: %s", message.Error.Code, message.Error.Message)
				}
				value.(chan callResult) <- result
			}
			continue
		}
		select {
		case c.events <- message:
		default:
			c.reportError(fmt.Errorf("codex event queue overflow"))
		}
	}
	if err := scanner.Err(); err != nil {
		c.reportError(fmt.Errorf("read codex app-server: %w", err))
	}
}

func (c *Client) call(ctx context.Context, method string, params any, target any) error {
	id := c.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	response := make(chan callResult, 1)
	c.pending.Store(key, response)
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.pending.Delete(key)
		return err
	}
	select {
	case <-ctx.Done():
		c.pending.Delete(key)
		return ctx.Err()
	case result := <-response:
		if result.err != nil {
			return result.err
		}
		if target == nil || len(result.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(result.result, target); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	case err := <-c.waitCh:
		return fmt.Errorf("codex app-server exited: %v: %s", err, strings.TrimSpace(c.stderr.String()))
	}
}

func (c *Client) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Client) respond(id json.RawMessage, result any) error {
	var decoded any
	if err := json.Unmarshal(id, &decoded); err != nil {
		return err
	}
	return c.write(map[string]any{"id": decoded, "result": result})
}

func (c *Client) write(message any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return fmt.Errorf("codex app-server is not running")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) handleServerRequest(ctx context.Context, message rpcMessage, emit func(agent.Event)) error {
	kind := "action"
	title := "Codex requests approval"
	var detail map[string]any
	_ = json.Unmarshal(message.Params, &detail)
	if strings.Contains(message.Method, "commandExecution") {
		kind = "command"
		title = "Approve Codex command?"
	} else if strings.Contains(message.Method, "fileChange") {
		kind = "file_change"
		title = "Approve Codex file change?"
	} else if strings.Contains(message.Method, "permissions") {
		kind = "permissions"
		title = "Approve additional Codex permissions?"
	}
	description := describeApproval(detail)
	request := &agent.ApprovalRequest{
		Agent: chat.Codex, Kind: kind, Title: title, Description: description,
		Path: stringValue(detail["cwd"]), Mode: chat.AccessReadWrite, Response: make(chan agent.ApprovalDecision, 1),
	}
	emit(agent.Event{Type: agent.EventApproval, Agent: chat.Codex, Approval: request})
	select {
	case <-ctx.Done():
		_ = c.respond(message.ID, approvalResponse(message.Method, detail, agent.CancelTurn))
		return ctx.Err()
	case decision := <-request.Response:
		return c.respond(message.ID, approvalResponse(message.Method, detail, decision))
	}
}

func approvalResponse(method string, detail map[string]any, decision agent.ApprovalDecision) map[string]any {
	if strings.Contains(method, "permissions/requestApproval") {
		permissions := map[string]any{}
		if decision == agent.ApproveOnce || decision == agent.ApproveSession {
			if requested, ok := detail["permissions"].(map[string]any); ok {
				permissions = requested
			}
		}
		scope := "turn"
		if decision == agent.ApproveSession {
			scope = "session"
		}
		return map[string]any{"permissions": permissions, "scope": scope}
	}
	return map[string]any{"decision": string(decision)}
}

func describeApproval(values map[string]any) string {
	for _, key := range []string{"reason", "command", "cwd", "path"} {
		if value := stringValue(values[key]); value != "" {
			return value
		}
	}
	data, _ := json.Marshal(values)
	return string(data)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, part := range typed {
			parts = append(parts, fmt.Sprint(part))
		}
		return strings.Join(parts, " ")
	default:
		if value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
}

func summarizeItem(raw json.RawMessage) string {
	var params struct {
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Status  string `json:"status"`
			Path    string `json:"path"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return ""
	}
	switch params.Item.Type {
	case "commandExecution":
		if params.Item.Command != "" {
			return "command: " + params.Item.Command
		}
		return "command " + params.Item.Status
	case "fileChange":
		if params.Item.Path != "" {
			return "file change: " + params.Item.Path
		}
		return "file change " + params.Item.Status
	case "mcpToolCall":
		return "MCP tool " + params.Item.Status
	}
	return ""
}

func (c *Client) interrupt(turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ignored any
	_ = c.call(ctx, "turn/interrupt", map[string]any{"threadId": c.threadID, "turnId": turnID}, &ignored)
}

func (c *Client) reportError(err error) {
	select {
	case c.errCh <- err:
	default:
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.stopProcess()
}

func (c *Client) stopProcess() error {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	err := c.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (c *Client) resetProcess() {
	c.mu.Lock()
	waitCh := c.waitCh
	_ = c.stopProcess()
	c.mu.Unlock()
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
	}

	c.mu.Lock()
	c.cmd = nil
	c.stdin = nil
	c.threadID = ""
	c.workspace = ""
	c.started = false
	c.pending.Range(func(key, _ any) bool {
		c.pending.Delete(key)
		return true
	})
	c.mu.Unlock()
	for {
		select {
		case <-c.events:
		case <-c.errCh:
		default:
			return
		}
	}
}
