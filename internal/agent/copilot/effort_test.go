package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

func TestCopilotEffortResetsSessionAndRestoresProviderDefaultOnWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan map[string]any, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			length := 0
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if line == "" {
					break
				}
				if value, ok := strings.CutPrefix(line, "Content-Length: "); ok {
					length, _ = strconv.Atoi(value)
				}
			}
			if length <= 0 {
				return
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			var request struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(body, &request) != nil {
				return
			}
			result := map[string]any{}
			switch request.Method {
			case "connect":
				result = map[string]any{"ok": true, "protocolVersion": 3, "version": "test"}
			case "session.create", "session.resume":
				requests <- request.Params
				result["sessionId"] = request.Params["sessionId"]
			}
			if request.ID == nil {
				continue
			}
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	native := sdk.NewClient(&sdk.ClientOptions{Connection: sdk.URIConnection{URL: listener.Addr().String()}, Mode: sdk.ModeEmpty})
	if err := native.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer native.Stop()
	client := New(Config{SessionID: "previous", Effort: "high", Permissions: chat.PermissionWorkspace})
	defer client.Close()
	for _, effort := range []string{"low", "auto", "high", ""} {
		if !client.Configure(chat.AgentSettings{Effort: effort, Permissions: chat.PermissionWorkspace}) || client.SessionID() != "" {
			t.Fatal("effort change did not invalidate the previous session")
		}
		session, err := client.openSession(ctx, native, client.config, agent.TurnRequest{Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		client.session, client.config.SessionID = session, session.SessionID
		select {
		case request := <-requests:
			if effort == "" || effort == "auto" {
				if _, present := request["reasoningEffort"]; present {
					t.Fatalf("provider default was sent as an override: %+v", request)
				}
			} else if request["reasoningEffort"] != effort {
				t.Fatalf("selected effort missing: %+v", request)
			}
		case <-ctx.Done():
			t.Fatal("no session request")
		}
	}
	_ = client.Close()
	_ = native.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("fixture did not close")
	}
}
