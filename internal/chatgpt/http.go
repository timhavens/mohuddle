package chatgpt

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
)

// ServePrivate exposes concurrent MCP requests only on a private Unix socket.
// The outbound tunnel supplies the remote authentication; the bridge continues
// to authenticate every operation with the dedicated, renewable local grant.
// No TCP port or public HTTP listener is opened.
func (b *Bridge) ServePrivate(ctx context.Context, socket string) (io.Closer, error) {
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("private MCP socket requires an absolute path")
	}
	dir, err := os.Stat(filepath.Dir(socket))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("private MCP socket requires a private directory")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("cannot open private MCP socket: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("cannot secure private MCP socket")
	}
	server := b.Server()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp" {
				http.NotFound(w, r)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, api.MaxFrameBytes)
			handler.ServeHTTP(w, r)
		})}
	stop := context.AfterFunc(ctx, func() { _ = httpServer.Close() })
	go func() {
		defer stop()
		_ = httpServer.Serve(listener)
	}()
	return httpServer, nil
}
