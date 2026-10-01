//go:build !windows

package chatgpt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/buildinfo"
	roomguidance "github.com/timhavens/mohuddle/internal/chatgpt/skills/mohuddle-room"
)

func TestCoordinationGuidanceDeliveredThroughMCPInitializationAndSchemas(t *testing.T) {
	client := mcpClient(t, &Bridge{})
	if client.InitializeResult().ServerInfo.Version != buildinfo.Version {
		t.Fatal("MCP reports a hardcoded server version")
	}
	if !strings.Contains(client.InitializeResult().Instructions, roomguidance.Coordination) {
		t.Fatal("MCP initialization omitted the shared coordination policy")
	}
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{"mohuddle_request_work": "continuation", "mohuddle_publish": "handoff_id", "mohuddle_coordinator_report": "waiting_on", "mohuddle_notification": "panel_id", "mohuddle_followups": "revision", "mohuddle_read": "client_contract_version"}
	for _, tool := range listed.Tools {
		data, _ := json.Marshal(tool.Meta)
		if strings.Contains(string(data), PanelURI) != (tool.Name == "mohuddle_panel") {
			t.Fatal("only mohuddle_panel may render a widget", tool.Name)
		}
		if tool.Name == "mohuddle_panel" {
			schema, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(schema), "replace_existing") {
				t.Fatal("explicit panel replacement input missing")
			}
		}
		if field, ok := fields[tool.Name]; ok {
			data, err := json.Marshal(tool.InputSchema)
			if err != nil || !strings.Contains(string(data), field) {
				t.Fatalf("%s schema missing %s: %s %v", tool.Name, field, data, err)
			}
			if tool.Name == "mohuddle_coordinator_report" && !strings.Contains(string(data), "handoff_only") {
				t.Fatal("scoped report field missing")
			}
			if tool.Name == "mohuddle_notification" {
				var schema struct {
					Required []string `json:"required"`
				}
				if err := json.Unmarshal(data, &schema); err != nil {
					t.Fatal(err)
				}
				for _, name := range schema.Required {
					if name == "result_id" {
						t.Fatal("panel availability has no result; its heartbeat must pass MCP schema validation")
					}
				}
			}
			delete(fields, tool.Name)
		}
	}
	if len(fields) > 0 {
		t.Fatal("missing tools", fields)
	}
	rendered := panelDocument()
	reminder, err := json.Marshal(roomguidance.Brief)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "__COORDINATION_REMINDER__") || !strings.Contains(rendered, string(reminder)) {
		t.Fatal("panel did not receive the shared reminder")
	}
	if roomguidance.Version == "" || roomguidance.Participant == "" {
		t.Fatal("incomplete shared guidance")
	}
}

func TestCoordinatorGuidanceReachesFreshRoomConversations(t *testing.T) {
	check := func(t *testing.T, client *mcp.ClientSession, conversation string, joined api.ChatGPTView) {
		t.Helper()
		views := []api.ChatGPTView{joined,
			roomToolValue[api.ChatGPTView](t, client, conversation, "mohuddle_read", api.ChatGPTReadRequest{ParticipationID: joined.ParticipationID}),
			roomToolValue[api.ChatGPTView](t, client, conversation, "mohuddle_panel", api.ChatGPTLeaveRequest{ParticipationID: joined.ParticipationID}),
		}
		for i, view := range views {
			if view.InstructionVersion != roomguidance.Version || !strings.Contains(view.Usage, roomguidance.Brief) {
				t.Fatalf("room view %d omitted current coordinator guidance", i)
			}
			if view.RoomID != joined.RoomID || view.ParticipationID != joined.ParticipationID {
				t.Fatalf("room view %d changed the selected attachment", i)
			}
		}
	}
	t.Run("single room", func(t *testing.T) {
		b, _, _, _ := testBridge(t)
		client := mcpClient(t, b)
		const conversation = "fresh-single-conversation"
		joined := roomToolValue[api.ChatGPTView](t, client, conversation, "mohuddle_join", JoinInput{})
		check(t, client, conversation, joined)
	})
	t.Run("separate conversations and rooms", func(t *testing.T) {
		b, _, _, _ := managerBridge(t)
		first, second := mcpClient(t, b), mcpClient(t, b)
		const firstChat, secondChat = "fresh-first-conversation", "fresh-second-conversation"
		joined := roomToolValue[api.ChatGPTView](t, first, firstChat, "mohuddle_join", JoinInput{Room: "Room 1"})
		created := roomToolValue[api.ChatGPTView](t, second, secondChat, "mohuddle_create_room", CreateRoomInput{OperationID: "guidance-second-room"})
		if joined.RoomID == created.RoomID || joined.ParticipationID == created.ParticipationID {
			t.Fatal("fresh conversations were not attached to separate rooms")
		}
		check(t, first, firstChat, joined)
		check(t, second, secondChat, created)
	})
}
