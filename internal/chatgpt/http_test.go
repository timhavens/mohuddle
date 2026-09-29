//go:build !windows

package chatgpt

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/testutil"
)

func TestPrivateMCPConcurrentRequests(t *testing.T) {
	dir := testutil.ShortTempDir(t)
	backend, err := net.Listen("unix", filepath.Join(dir, "room.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
				for {
					var req api.Request
					if decoder.Decode(&req) != nil {
						return
					}
					var result any = api.HelloResult{Kind: api.ClientChatGPT}
					if req.Type == "chatgpt.read" {
						var input api.ChatGPTReadRequest
						if json.Unmarshal(req.Payload, &input) != nil {
							return
						}
						if input.ParticipationID == "slow-room" {
							close(started)
							<-release
						}
						result = api.ChatGPTView{ParticipationID: input.ParticipationID, RoomID: input.ParticipationID, Messages: []api.ChatGPTMessage{}}
					}
					if encoder.Encode(api.Response{Version: api.Version, ID: req.ID, OK: true, Result: result}) != nil {
						return
					}
				}
			}()
		}
	}()
	b := New(api.ChatGPTConnection{Socket: backend.Addr().String()})
	socket := filepath.Join(dir, "mcp.sock")
	server, err := b.ServePrivate(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("MCP socket is not private", err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	connect := func() *mcp.ClientSession {
		c, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: "http://localhost/mcp", HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	a, c := connect(), connect()
	slowDone := make(chan error, 1)
	go func() {
		_, err := a.CallTool(t.Context(), &mcp.CallToolParams{Name: "mohuddle_read", Arguments: api.ChatGPTReadRequest{ParticipationID: "slow-room", WaitSeconds: 25}})
		slowDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("slow request never reached room")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	fast, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "mohuddle_read", Arguments: api.ChatGPTReadRequest{ParticipationID: "fast-room"}})
	if err != nil || fast.IsError {
		t.Fatal("one room's wait blocked the other room", err)
	}
	value, _ := json.Marshal(fast.StructuredContent)
	var view api.ChatGPTView
	if json.Unmarshal(value, &view) != nil || view.RoomID != "fast-room" {
		t.Fatal("concurrent response crossed room identity")
	}
	select {
	case <-slowDone:
		t.Fatal("slow request ended before release")
	default:
	}
	// Closing the private endpoint must cancel an outstanding request promptly.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-slowDone:
	case <-time.After(2 * time.Second):
		t.Fatal("closing bridge left request waiting")
	}
}

func TestPrivateMCPRejectsPublicDirectory(t *testing.T) {
	dir := testutil.ShortTempDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if server, err := New(api.ChatGPTConnection{}).ServePrivate(t.Context(), filepath.Join(dir, "mcp.sock")); err == nil {
		server.Close()
		t.Fatal("public socket directory accepted")
	}
}
