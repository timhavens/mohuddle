//go:build !windows

package chatgpt

import (
	"encoding/json"
	"strings"
	"testing"

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
		if tool.Name == "mohuddle_join" || tool.Name == "mohuddle_panel" {
			data, _ := json.Marshal(tool.Meta)
			if !strings.Contains(string(data), PanelURI) {
				t.Fatal("panel must render on join and reopen", tool.Name)
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
	if strings.Contains(rendered, "__COORDINATION_REMINDER__") || !strings.Contains(rendered, roomguidance.Brief) {
		t.Fatal("panel did not receive the shared reminder")
	}
	if roomguidance.Version == "" || roomguidance.Participant == "" {
		t.Fatal("incomplete shared guidance")
	}
}
