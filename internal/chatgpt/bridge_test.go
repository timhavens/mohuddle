//go:build !windows

package chatgpt

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/testutil"
)

type respondingPeer struct{ participant chat.Participant }

func (p respondingPeer) Participant() chat.Participant { return p.participant }
func (respondingPeer) Close() error                    { return nil }
func (p respondingPeer) Run(_ context.Context, input agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
	return agent.TurnResult{Text: string(p.participant) + " has additional evidence", Done: true}, nil
}

func testBridge(t *testing.T) (*Bridge, *api.Service, *room.Orchestrator, api.Credentials) {
	t.Helper()
	root := testutil.ShortTempDir(t)
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	o, err := room.New(r, nil, s, respondingPeer{chat.Codex}, respondingPeer{chat.Claude})
	if err != nil {
		t.Fatal(err)
	}
	o.ConfigureTemporaryAgents(nil)
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	service, err := api.NewService(*credentials, o)
	if err != nil {
		t.Fatal(err)
	}
	local, err := api.StartLocal(filepath.Join(root, "api.sock"), service, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureChatGPT(local.Addr(), filepath.Join(root, "chatgpt.json"), nil)
	path, err := service.EnableChatGPT(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.RevokeChatGPT(); _ = local.Close(); _ = o.Close() })
	return bridge, service, o, *credentials
}

func mcpClient(t *testing.T, b *Bridge) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := b.Server().Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test-chatgpt", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func callMCP[T any](t *testing.T, client *mcp.ClientSession, name string, args any) T {
	t.Helper()
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: mcp.Meta{"openai/session": "private-conversation-session"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %+v", name, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestMCPRoomExchangeAndRevocation(t *testing.T) {
	b, service, o, _ := testBridge(t)
	if err := b.Doctor(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, messages := o.Snapshot()
	if state.Present(chat.ChatGPT) || len(messages) != 0 {
		t.Fatal("doctor joined or touched transcript")
	}
	client := mcpClient(t, b)
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 7 {
		t.Fatalf("unexpected exposed tools: %+v", tools.Tools)
	}
	for _, tool := range tools.Tools {
		if !strings.HasPrefix(tool.Name, "mohuddle_") || tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint != (tool.Name == "mohuddle_request_work") {
			t.Fatal("invalid capability metadata")
		}
		if tool.Name == "mohuddle_request_work" && (tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Fatal("work tool understated its write capability")
		}
	}
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if !view.State.Connected || len(view.Participants) != 2 {
		t.Fatalf("room join: %+v", view)
	}
	if err := o.Post("@chatgpt Please discuss the shared conclusion"); err != nil {
		t.Fatal(err)
	}
	view = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter})
	if len(view.Messages) != 1 {
		t.Fatal("human message did not reach ChatGPT")
	}
	input := api.ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "first-contribution", Text: "This is the selected shared conclusion", ReplyTo: view.Messages[0].Sequence, RequestReplies: []chat.Participant{chat.Codex, chat.Claude}}
	publication := callMCP[PublishOutput](t, client, "mohuddle_publish", input)
	duplicate := callMCP[PublishOutput](t, client, "mohuddle_publish", input)
	if !duplicate.Duplicate || duplicate.Sequence != publication.Sequence {
		t.Fatal("MCP retry added another contribution")
	}
	seen := map[chat.Participant]bool{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 2 {
		view = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter, WaitSeconds: 1})
		for _, message := range view.Messages {
			if message.Author.ValidAgent() && message.ReplyTo == publication.Sequence {
				seen[message.Author] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Fatal("selected peers did not reply through MCP")
	}
	callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "synthesis", Text: "The peer evidence supports the refined result"})
	callMCP[api.ChatGPTView](t, client, "mohuddle_panel", api.ChatGPTLeaveRequest{ParticipationID: view.ParticipationID})
	resource, err := client.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: PanelURI})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(resource)
	if len(resource.Contents) != 1 || resource.Contents[0].MIMEType != "text/html;profile=mcp-app" || !strings.Contains(string(data), "connectDomains") || strings.Contains(string(data), b.connection.Token) || strings.Contains(string(data), b.connection.Socket) {
		t.Fatal("panel metadata or private data boundary failed")
	}
	_, messages = o.Snapshot()
	if len(messages) != 5 {
		t.Fatalf("unexpected shared transcript size: %d", len(messages))
	}
	for _, message := range messages {
		if strings.Contains(message.Text, "private-conversation-session") {
			t.Fatal("private ChatGPT metadata entered room transcript")
		}
	}
	if err := service.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_read", Arguments: api.ChatGPTReadRequest{ParticipationID: view.ParticipationID}})
	if err == nil && !result.IsError {
		t.Fatal("live MCP server retained revoked access")
	}
}

func TestMCPReloadsRenewedRoomGrant(t *testing.T) {
	b, service, _, _ := testBridge(t)
	stale := New(b.connection)
	client := mcpClient(t, b)
	first := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if _, err := service.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := stale.Doctor(t.Context()); err == nil {
		t.Fatal("old room grant remained authorized after renewal")
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_read", Arguments: api.ChatGPTReadRequest{ParticipationID: first.ParticipationID}})
	if err == nil && !result.IsError {
		t.Fatal("renewal retained the old participation")
	}
	second := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if !second.State.Connected || second.ParticipationID == first.ParticipationID {
		t.Fatal("existing MCP session could not rejoin with the renewed grant")
	}
	callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{ParticipationID: second.ParticipationID, OperationID: "after-renewal", Text: "Contribution after the host renewed access"})
	if err := service.RevokeChatGPT(); err != nil {
		t.Fatal(err)
	}
	result, err = client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_join", Arguments: JoinInput{ConversationKey: "renewed-conversation"}})
	if err == nil && !result.IsError {
		t.Fatal("revocation did not block the existing MCP session")
	}
	if _, err := service.EnableChatGPT(time.Hour); err != nil {
		t.Fatal(err)
	}
	third := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if !third.State.Connected || third.ParticipationID == second.ParticipationID {
		t.Fatal("explicit host reauthorization did not allow a fresh participation")
	}
}

func TestBridgeReloadFailsClosed(t *testing.T) {
	other, _, otherRoom, _ := testBridge(t)
	for _, scenario := range []string{"different room", "different socket", "expired", "unsafe permissions", "missing", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			b, _, _, _ := testBridge(t)
			connection := b.connection
			switch scenario {
			case "different room":
				connection.RoomID = other.connection.RoomID
			case "different socket":
				connection.Socket = other.connection.Socket
			case "expired":
				connection.ExpiresAt = time.Now().Add(-time.Minute)
			}
			data, err := json.Marshal(connection)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b.connectionPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unsafe permissions":
				if err := os.Chmod(b.connectionPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "missing", "symlink":
				if err := os.Remove(b.connectionPath); err != nil {
					t.Fatal(err)
				}
				if scenario == "symlink" {
					target := filepath.Join(filepath.Dir(b.connectionPath), "replaced.json")
					if err := os.WriteFile(target, data, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, b.connectionPath); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = b.Doctor(t.Context())
			if err == nil || !strings.Contains(err.Error(), "authentication_failed") {
				t.Fatalf("changed connection was not rejected: %v", err)
			}
			if strings.Contains(err.Error(), b.connection.Token) || strings.Contains(err.Error(), b.connection.Socket) || strings.Contains(err.Error(), b.connectionPath) {
				t.Fatal("connection error disclosed private host details")
			}
		})
	}
	state, messages := otherRoom.Snapshot()
	if state.Present(chat.ChatGPT) || len(messages) != 0 {
		t.Fatal("reload reached another room")
	}
}

func TestMCPRejectsUnexposedControlsAndUnexpectedFields(t *testing.T) {
	b, _, _, _ := testBridge(t)
	client := mcpClient(t, b)
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	for _, call := range []*mcp.CallToolParams{
		{Name: "command.invoke", Arguments: map[string]any{"command": "stop"}},
		{Name: "mohuddle_publish", Arguments: map[string]any{"participation_id": view.ParticipationID, "operation_id": "op", "text": "selected text", "private_chat": "must never enter room"}},
		{Name: "mohuddle_read", Arguments: map[string]any{"participation_id": view.ParticipationID, "after": -1}},
		{Name: "mohuddle_publish", Arguments: api.ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "op", Text: "selected text", RequestReplies: []chat.Participant{chat.ChatGPT}}},
	} {
		result, err := client.CallTool(t.Context(), call)
		if err == nil && !result.IsError {
			t.Fatalf("invalid tool call accepted: %s", call.Name)
		}
	}
}

func TestBridgeCannotUseOrdinaryLocalCredentials(t *testing.T) {
	b, _, _, credentials := testBridge(t)
	connection := b.connection
	connection.Token = credentials.Entries[0].Token
	data, err := json.Marshal(connection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.connectionPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bridge := range []*Bridge{b, New(connection)} {
		if err := bridge.Doctor(t.Context()); err == nil || !strings.Contains(err.Error(), "dedicated ChatGPT") {
			t.Fatalf("bridge accepted administrator credential: %v", err)
		}
	}
}

func TestMCPDelegatesWorkAndReadsCompletion(t *testing.T) {
	b, _, _, _ := testBridge(t)
	client := mcpClient(t, b)
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	input := api.ChatGPTWorkRequest{ParticipationID: view.ParticipationID, OperationID: "work-handoff", Target: chat.Codex, Text: "Perform the user's requested edit within existing permissions."}
	work := callMCP[WorkOutput](t, client, "mohuddle_request_work", input)
	if work.WorkflowID == "" || work.Sequence == 0 || work.Duplicate {
		t.Fatal("MCP work was not accepted")
	}
	duplicate := callMCP[WorkOutput](t, client, "mohuddle_request_work", input)
	if !duplicate.Duplicate || duplicate.WorkflowID != work.WorkflowID {
		t.Fatal("MCP retry duplicated work")
	}
	foundReply, completed := false, false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!foundReply || !completed) {
		view = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter, WaitSeconds: 1})
		for _, message := range view.Messages {
			if message.Author == chat.Codex && message.WorkflowID == work.WorkflowID {
				foundReply = true
			}
		}
		for _, status := range view.Work {
			if status.WorkflowID == work.WorkflowID && status.State == chat.WorkflowCompleted {
				completed = true
			}
		}
	}
	if !foundReply || !completed {
		t.Fatal("work result or completion status did not reach ChatGPT")
	}
}

func TestMCPRejectsComposerCommandsWithoutSideEffects(t *testing.T) {
	b, _, o, _ := testBridge(t)
	client := mcpClient(t, b)
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	for _, command := range []string{"/round", "/ask", "/delegate"} {
		result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_publish", Arguments: api.ChatGPTPublishRequest{
			ParticipationID: view.ParticipationID, OperationID: "mistaken-command", Text: command + " please discuss this", RequestReplies: []chat.Participant{chat.Codex},
		}})
		if err != nil || !result.IsError {
			t.Fatalf("command accepted: %s, %v", command, err)
		}
		data, _ := json.Marshal(result.StructuredContent)
		var receipt PublishOutput
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.FailureCode != "composer_command_not_supported" || receipt.MessagePosted == nil || *receipt.MessagePosted || receipt.AgentScheduled == nil || *receipt.AgentScheduled || !strings.Contains(receipt.FailureReason, "mohuddle_") {
			t.Fatalf("missing corrective receipt: %+v", receipt)
		}
	}
	state, messages := o.Snapshot()
	if len(messages) != 0 || len(state.Workflows) != 0 || len(state.Conversations) != 0 {
		t.Fatal("rejected command had side effects")
	}
	// Quoting command syntax in ordinary discussion remains possible.
	post := callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{ParticipationID: view.ParticipationID, OperationID: "syntax-example", Text: "The composer supports `/round`; this is an explanation."})
	if post.Action != "post" || post.AgentScheduled == nil || *post.AgentScheduled {
		t.Fatal("literal example dispatched")
	}
}

func TestMCPDraftThenRoundUsesSeparateTrackedOperations(t *testing.T) {
	b, _, _, _ := testBridge(t)
	client := mcpClient(t, b)
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	if view.Usage == "" {
		t.Fatal("joined without operational guidance")
	}
	draft := callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{
		ParticipationID: view.ParticipationID, OperationID: "draft-stage", Text: "Prepare a text-only proposal for review", RequestReplies: []chat.Participant{chat.Codex},
	})
	if draft.Action != "replies" || draft.NextAction == "" {
		t.Fatal("draft receipt did not identify its operation")
	}
	var actualDraft api.ChatGPTMessage
	settled := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!settled || actualDraft.Sequence == 0) {
		view = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter, WaitSeconds: 1})
		for _, message := range view.Messages {
			if message.Author == chat.Codex && message.ReplyTo == draft.Sequence {
				actualDraft = message
			}
		}
		for _, result := range view.ReplyResults {
			if result.SourceSequence == draft.Sequence && result.Participant == chat.Codex && result.State.Terminal() {
				settled = true
			}
		}
	}
	if !settled || actualDraft.Sequence == 0 {
		t.Fatal("draft result was not observable")
	}
	input := api.ChatGPTRoundRequest{ParticipationID: view.ParticipationID, OperationID: "review-stage", Text: "Review this exact draft: " + actualDraft.Text, ReplyTo: actualDraft.Sequence, Participants: []chat.Participant{chat.Codex, chat.Claude}}
	round := callMCP[WorkOutput](t, client, "mohuddle_request_round", input)
	if round.Action != "round" || round.WorkflowID == "" || round.Moderator != chat.Codex || len(round.ScheduledAgents) != 2 || round.ScheduledAgents[1] != chat.Codex {
		t.Fatalf("round receipt: %+v", round)
	}
	duplicate := callMCP[WorkOutput](t, client, "mohuddle_request_round", input)
	if !duplicate.Duplicate || duplicate.WorkflowID != round.WorkflowID {
		t.Fatal("round retry duplicated the operation")
	}
	completed := false
	seen := map[chat.Participant]bool{}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!completed || len(seen) < 2) {
		view = callMCP[api.ChatGPTView](t, client, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: view.ParticipationID, After: view.NextAfter, WaitSeconds: 1})
		for _, status := range view.Work {
			if status.WorkflowID == round.WorkflowID && status.Kind == "round" && status.State == chat.WorkflowCompleted {
				completed = true
			}
		}
		for _, message := range view.Messages {
			if message.WorkflowID == round.WorkflowID && message.Author.ValidAgent() {
				seen[message.Author] = true
			}
		}
	}
	if !completed || len(seen) != 2 || view.Usage == "" {
		t.Fatal("round completion, actual reviews, or current guidance missing")
	}
}

func TestMCPReceiptsDistinguishPostsRepliesAndWork(t *testing.T) {
	b, _, o, _ := testBridge(t)
	client := mcpClient(t, b)
	view := callMCP[api.ChatGPTView](t, client, "mohuddle_join", JoinInput{})
	plain := callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{
		ParticipationID: view.ParticipationID, OperationID: "text-only", Text: "@CODEX @codex /ask /round /delegate: make the edit",
	})
	if plain.MessagePosted == nil || !*plain.MessagePosted || plain.AgentScheduled == nil || *plain.AgentScheduled || len(plain.ScheduledAgents) != 0 {
		t.Fatal("plain post claimed to dispatch an agent")
	}
	state, _ := o.Snapshot()
	if len(state.Workflows) != 0 || len(state.Conversations) != 0 {
		t.Fatal("mention or slash command dispatched")
	}
	reply := callMCP[PublishOutput](t, client, "mohuddle_publish", api.ChatGPTPublishRequest{
		ParticipationID: view.ParticipationID, OperationID: "feedback", Text: "Please review", RequestReplies: []chat.Participant{chat.Codex},
	})
	if reply.MessagePosted == nil || !*reply.MessagePosted || reply.AgentScheduled == nil || !*reply.AgentScheduled || len(reply.ScheduledAgents) != 1 || reply.ScheduledAgents[0] != chat.Codex {
		t.Fatal("feedback receipt omitted dispatch")
	}
	work := callMCP[WorkOutput](t, client, "mohuddle_request_work", api.ChatGPTWorkRequest{
		ParticipationID: view.ParticipationID, OperationID: "edit", Target: chat.Claude, Text: "Make the requested edit",
	})
	if work.MessagePosted == nil || !*work.MessagePosted || work.AgentScheduled == nil || !*work.AgentScheduled || work.WorkflowID == "" || len(work.ScheduledAgents) != 1 || work.ScheduledAgents[0] != chat.Claude {
		t.Fatal("work receipt omitted dispatch")
	}
	failed, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_request_work", Arguments: api.ChatGPTWorkRequest{
		ParticipationID: view.ParticipationID, OperationID: "absent", Target: chat.Agy, Text: "Make the requested edit",
	}})
	if err != nil || !failed.IsError {
		t.Fatalf("absent target: %v", err)
	}
	data, _ := json.Marshal(failed.StructuredContent)
	var receipt WorkOutput
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.MessagePosted == nil || *receipt.MessagePosted || receipt.AgentScheduled == nil || *receipt.AgentScheduled || receipt.FailureCode != "work_failed" || receipt.FailureReason == "" {
		t.Fatal("failure did not explain what was posted and dispatched")
	}
}

func TestMCPPartialFailureAndLostResponseRetainOutcome(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "posted-but-not-scheduled", true: "response-lost"}[lost], func(t *testing.T) {
			socket := filepath.Join(testutil.ShortTempDir(t), "room.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
				var request api.Request
				if decoder.Decode(&request) != nil {
					return
				}
				_ = encoder.Encode(api.Response{Version: api.Version, ID: request.ID, OK: true, Result: api.HelloResult{Kind: api.ClientChatGPT}})
				if decoder.Decode(&request) != nil || lost {
					return
				}
				_ = encoder.Encode(api.Response{Version: api.Version, ID: request.ID, OK: false,
					Error:  &api.ProtocolError{Code: "publish_failed", Message: "contribution saved but peer replies could not be scheduled"},
					Result: map[string]any{"sequence": 7, "message_posted": true, "agent_scheduled": false, "scheduled_agents": []string{}, "duplicate": false, "exchanges_remaining": 7},
				})
			}()
			b := New(api.ChatGPTConnection{Version: api.ChatGPTConnectionVersion, Socket: socket, RoomID: "test-room", Token: "test-only", ExpiresAt: time.Now().Add(time.Hour)})
			client := mcpClient(t, b)
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_publish", Arguments: api.ChatGPTPublishRequest{ParticipationID: "test", OperationID: "original-id", Text: "Review this", RequestReplies: []chat.Participant{chat.Codex}}})
			if err != nil || !result.IsError {
				t.Fatalf("failure not reported: %v", err)
			}
			data, _ := json.Marshal(result.StructuredContent)
			var receipt PublishOutput
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			if lost {
				if !receipt.OutcomeUnknown || receipt.MessagePosted != nil || receipt.AgentScheduled != nil || receipt.FailureCode != "connection_error" {
					t.Fatal("lost response claimed a known mutation outcome")
				}
			} else if receipt.OutcomeUnknown || receipt.MessagePosted == nil || !*receipt.MessagePosted || receipt.AgentScheduled == nil || *receipt.AgentScheduled || receipt.Sequence != 7 || receipt.FailureCode != "publish_failed" {
				t.Fatal("partial failure lost the saved-message receipt")
			}
			<-done
		})
	}
}
