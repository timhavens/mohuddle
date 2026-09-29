//go:build !windows

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOwnedProcessStopsBridgeAndKeepsLock(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "bridge.pid")
	script := filepath.Join(dir, "fake-tunnel")
	// Both shell and bridge are in the owned process group. The shell reaps the
	// bridge on TERM; no live tunnel or account credentials are used.
	body := "#!/bin/sh\ntrap 'wait; exit 0' TERM\nsleep 300 &\necho $! > " + quoteCommandArg(pidFile) + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "lock")
	lock, err := lockTunnel(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	child, err := startProcess(context.Background(), launch{binary: script, env: os.Environ(), lock: lock})
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	defer child.Stop()
	lock.Close()
	deadline := time.Now().Add(3 * time.Second)
	bridgePID := 0
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			bridgePID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if bridgePID > 0 {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if bridgePID == 0 {
		t.Fatal("bridge did not start")
	}
	if other, err := lockTunnel(lockPath); err == nil {
		other.Close()
		t.Fatal("child did not retain ownership after parent descriptor closed")
	}
	child.Stop()
	if err := syscall.Kill(bridgePID, 0); err == nil {
		t.Fatal("bridge still running after stop")
	}
	other, err := lockTunnel(lockPath)
	if err != nil {
		t.Fatal("ownership not released after shutdown:", err)
	}
	other.Close()
}

func TestHealthRequiresSuccessfulCLIReport(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-health")
	urlFile := filepath.Join(dir, "health.url")
	if err := os.WriteFile(urlFile, []byte("http://127.0.0.1:12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The official v0.0.14 health command returns result "ok", unlike doctor's
	// "pass" result. Keep this contract independent of our implementation:
	// https://github.com/openai/tunnel-client/blob/v0.0.14/cmd/client/health_command.go
	for _, tc := range []struct {
		name, body string
		exit       int
		want       bool
	}{
		{"healthy", `{"result":"ok","healthz":{"ok":true},"readyz":{"ok":true},"control_plane_poll":{"ok":true}}`, 0, true},
		{"poll not ready", `{"result":"ok","healthz":{"ok":true},"readyz":{"ok":true},"control_plane_poll":{"ok":false}}`, 0, false},
		{"unhealthy", `{"result":"ok","healthz":{"ok":false},"readyz":{"ok":true},"control_plane_poll":{"ok":true}}`, 0, false},
		{"not ready", `{"result":"ok","healthz":{"ok":true},"readyz":{"ok":false},"control_plane_poll":{"ok":true}}`, 0, false},
		{"missing poll", `{"result":"ok","healthz":{"ok":true},"readyz":{"ok":true}}`, 0, false},
		{"failed result", `{"result":"fail","healthz":{"ok":true},"readyz":{"ok":true},"control_plane_poll":{"ok":true}}`, 0, false},
		{"unexpected result", `{"result":"pass","healthz":{"ok":true},"readyz":{"ok":true},"control_plane_poll":{"ok":true}}`, 0, false},
		{"failed command", `{"result":"ok","healthz":{"ok":true},"readyz":{"ok":true},"control_plane_poll":{"ok":true}}`, 2, false},
		{"invalid json", `not json`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := "#!/bin/sh\n[ \"$1\" = health ] || exit 1\ncase \"$*\" in *--pid*--require-control-plane-poll*--json*) ;; *) exit 1;; esac\nprintf '%s' " + quoteCommandArg(tc.body) + "\nexit " + strconv.Itoa(tc.exit) + "\n"
			if err := os.WriteFile(script, []byte(code), 0o700); err != nil {
				t.Fatal(err)
			}
			if got := probeHealth(t.Context(), launch{binary: script, health: urlFile, env: os.Environ()}, 123); got != tc.want {
				t.Fatalf("health result %t; want %t", got, tc.want)
			}
		})
	}
}

func TestInstalledTunnelClientHealthContract(t *testing.T) {
	binary, err := exec.LookPath("tunnel-client")
	if err != nil {
		t.Skip("optional installed-client compatibility check")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skip("sandbox blocks the local fixture listener")
	}
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the actual health CLI against a local fixture, without starting
	// a tunnel or using an OpenAI account. This catches output-contract changes
	// that a fake health executable alone cannot detect.
	var polled, ready atomic.Bool
	ready.Store(true)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprintln(w, "live")
		case "/readyz":
			if !ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			fmt.Fprintln(w, "ready")
		case "/metrics":
			value := 0
			if polled.Load() {
				value = 1
			}
			fmt.Fprintf(w, "commands_poll_last_successful_timestamp_seconds %d\n", value)
		default:
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	urlFile := filepath.Join(t.TempDir(), "health.url")
	if err := os.WriteFile(urlFile, []byte("http://"+listener.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}
	value := launch{binary: binary, health: urlFile, env: []string{}}
	for _, tc := range []struct {
		name                string
		polled, ready, want bool
	}{
		{"waiting for first poll", false, true, false},
		{"ready after successful poll", true, true, true},
		{"not ready despite earlier poll", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			polled.Store(tc.polled)
			ready.Store(tc.ready)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if got := probeHealth(ctx, value, os.Getpid()); got != tc.want {
				t.Fatalf("installed tunnel-client health result %t; want %t", got, tc.want)
			}
		})
	}
}

func TestGeneratedConfigAcceptedByInstalledTunnelProxy(t *testing.T) {
	binary, err := exec.LookPath("tunnel-client")
	if err != nil {
		t.Skip("optional installed-client compatibility check")
	}
	t.Setenv("MOHUDDLE_TEST_KEY", "test-placeholder-not-a-real-key")
	p := profile{ControlPlane: map[string]any{"tunnel_id": "tunnel_0123456789abcdef0123456789abcdef", "api_key": "env:MOHUDDLE_TEST_KEY"}}
	dir, err := os.MkdirTemp("", "mcp-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "mcp.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	started, release := make(chan struct{}), make(chan struct{})
	type probeInput struct {
		Wait bool `json:"wait"`
	}
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "probe"}, func(ctx context.Context, _ *mcp.CallToolRequest, input probeInput) (*mcp.CallToolResult, map[string]bool, error) {
		if input.Wait {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		return nil, map[string]bool{"ok": true}, nil
	})

	server := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	path, err := p.writeConfig(dir, socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	infoPath := filepath.Join(dir, "proxy.json")
	// The local proxy exercises the real runtime HTTP/Unix-socket route. The
	// v0.0.14 doctor ignores unix_socket when probing reachability and OAuth.
	cmd := exec.CommandContext(ctx, binary, "dev", "proxy", "--backend", "go", "--profile-file", path, "--url-file", infoPath, "--duration", "15s")
	cmd.Env = tunnelEnvironment(p.ControlPlane)
	log, err := os.Create(filepath.Join(dir, "proxy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	var info struct {
		URL string `json:"mcp_url"`
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		data, _ := os.ReadFile(infoPath)
		if json.Unmarshal(data, &info) == nil && info.URL != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.URL == "" {
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("local tunnel never became ready: %s", data)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: info.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal("installed tunnel could not reach private MCP endpoint", err)
	}
	defer client.Close()
	if _, err := client.ListTools(ctx, nil); err != nil {
		t.Fatal("installed tunnel did not relay MCP", err)
	}

	defer close(release)
	waiting := make(chan error, 1)
	go func() {
		_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "probe", Arguments: probeInput{Wait: true}})
		waiting <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting call did not arrive")
	}
	fastCtx, stopFast := context.WithTimeout(ctx, 2*time.Second)
	defer stopFast()
	result, err := client.CallTool(fastCtx, &mcp.CallToolParams{Name: "probe", Arguments: probeInput{}})
	if err != nil || result.IsError {
		t.Fatal("installed tunnel serialized independent requests", err)
	}
	select {
	case <-waiting:
		t.Fatal("waiting call unexpectedly completed")
	default:
	}

}

func TestFailureHintsNeverReturnSubprocessSecrets(t *testing.T) {
	for _, output := range []string{"Unauthorized Bearer sk-private-secret", "parse config api_key: sk-private-secret", "unknown flag --private-secret", "some unknown error sk-private-secret"} {
		if strings.Contains(failureHint(output), "private-secret") {
			t.Fatal("raw diagnostic leaked")
		}
	}
}
