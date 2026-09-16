package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type State string

const (
	Stopped    State = "stopped"
	Starting   State = "connecting"
	Ready      State = "ready"
	Recovering State = "reconnecting"
	Failed     State = "failed"
	Expired    State = "expired"
)

type Status struct {
	State    State
	Detail   string
	Profile  string
	PID      int
	Restarts int
}

type Options struct {
	// RuntimeDir must be common to all rooms for this OS user; the tunnel ID
	// lock prevents two rooms from draining the same OpenAI tunnel.
	RuntimeDir, ProfileDir, Executable, Binary string
	Authorized                                 func() bool
	ProbeRoom                                  func(context.Context, string) error
	// Short timings and a fake process/health runner keep tests offline.
	pollInterval, unhealthyTimeout, retryDelay time.Duration
	start                                      func(context.Context, launch) (process, error)
	probe                                      func(context.Context, launch, int) bool
	foreign                                    func(context.Context, profile, string) bool
}

type launch struct {
	binary, config, health string
	tunnelID               string
	env                    []string
	lock                   *os.File
}
type process interface {
	PID() int
	Done() <-chan struct{}
	Stop()
}

// Manager has one worker and one desired state. Cancelling superseded work is
// immediate; teardown is serialized before a replacement can start. The TUI
// never waits for network checks, process exit, or retry backoff.
type Manager struct {
	mu                  sync.Mutex
	opts                Options
	status              Status
	profile, connection string
	wanted, closed      bool
	generation          uint64
	cancel              context.CancelFunc
	wake                chan struct{}
	done                chan struct{}
}

func New(opts Options) *Manager {
	if opts.Binary == "" {
		opts.Binary = "tunnel-client"
	}
	if opts.pollInterval == 0 {
		opts.pollInterval = 5 * time.Second
	}
	if opts.unhealthyTimeout == 0 {
		opts.unhealthyTimeout = 90 * time.Second
	}
	if opts.retryDelay == 0 {
		opts.retryDelay = 2 * time.Second
	}
	if opts.start == nil {
		opts.start = startProcess
	}
	if opts.probe == nil {
		opts.probe = probeHealth
	}
	if opts.foreign == nil {
		opts.foreign = foreignTunnel
	}
	m := &Manager{opts: opts, status: Status{State: Stopped}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go m.loop()
	return m
}

func (m *Manager) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status }

func (m *Manager) Start(profile, connection string)   { m.request(profile, connection, false) }
func (m *Manager) Restart(profile, connection string) { m.request(profile, connection, true) }

func (m *Manager) request(profile, connection string, restart bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if profile == "" {
		profile = DefaultProfile
	}
	if !restart && m.wanted && m.profile == profile && m.connection == connection && m.status.State != Failed && m.status.State != Expired {
		return
	}
	m.profile, m.connection, m.wanted = profile, connection, true
	m.status = Status{State: Starting, Profile: profile, Detail: "checking private room and tunnel"}
	m.changedLocked()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.wanted = false
	m.status = Status{State: Stopped, Profile: m.profile, Detail: "stopped by host"}
	m.changedLocked()
}

func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed, m.wanted = true, false
		m.changedLocked()
	}
	m.mu.Unlock()
	<-m.done
}

func (m *Manager) changedLocked() {
	m.generation++
	if m.cancel != nil {
		m.cancel()
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	defer close(m.done)
	for range m.wake {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		if !m.wanted {
			m.mu.Unlock()
			continue
		}
		generation, name, connection := m.generation, m.profile, m.connection
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.mu.Unlock()
		err := m.run(ctx, generation, name, connection)
		cancel()
		m.mu.Lock()
		if generation == m.generation {
			m.cancel = nil
			m.wanted = false
			m.status.PID = 0
			if err != nil {
				m.status.State, m.status.Detail = Failed, err.Error()
			}
		}
		m.mu.Unlock()
	}
}

func (m *Manager) update(generation uint64, state State, detail string, pid, retries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation == m.generation {
		m.status = Status{state, detail, m.profile, pid, retries}
	}
}

func (m *Manager) authorized() bool { return m.opts.Authorized != nil && m.opts.Authorized() }

func (m *Manager) run(ctx context.Context, generation uint64, name, connection string) error {
	if !m.authorized() {
		m.update(generation, Expired, "room access ended; use /join @chatgpt", 0, 0)
		return nil
	}
	dir := m.opts.ProfileDir
	if dir == "" {
		var err error
		dir, err = DefaultProfileDir()
		if err != nil {
			return fmt.Errorf("cannot locate tunnel profiles")
		}
	}
	p, err := readProfile(dir, name)
	if err != nil {
		return err
	}
	binary, err := exec.LookPath(m.opts.Binary)
	if err != nil {
		return fmt.Errorf("tunnel-client is not installed or not on MoHuddle's PATH; install the official client")
	}
	if m.opts.RuntimeDir == "" {
		return fmt.Errorf("private tunnel state directory is unavailable")
	}
	if err := os.MkdirAll(m.opts.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("cannot create private tunnel state directory")
	}
	if err := os.Chmod(m.opts.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("cannot secure tunnel state directory")
	}
	hash := sha256.Sum256([]byte(p.id))
	lock, err := lockTunnel(filepath.Join(m.opts.RuntimeDir, hex.EncodeToString(hash[:16])+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if ctx.Err() != nil {
		return nil
	}
	if m.opts.foreign(ctx, p, name) {
		return fmt.Errorf("a separately started tunnel is using this profile; stop it with Ctrl+C in its terminal (for a managed runtime: tunnel-client runtimes stop %s), then /chatgpt restart", name)
	}
	if m.opts.ProbeRoom != nil {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := m.opts.ProbeRoom(probe, connection)
		cancel()
		if err != nil {
			return fmt.Errorf("private room connection is unavailable; check /chatgpt status and rejoin")
		}
	}
	work, err := os.MkdirTemp(m.opts.RuntimeDir, "run-")
	if err != nil {
		return fmt.Errorf("cannot create private tunnel runtime")
	}
	defer os.RemoveAll(work)
	config, err := p.writeConfig(work, m.opts.Executable, connection)
	if err != nil {
		return err
	}
	launch := launch{binary: binary, config: config, health: filepath.Join(work, "health.url"), tunnelID: p.id, env: tunnelEnvironment(p.ControlPlane), lock: lock}
	for retries := 0; retries <= 3; retries++ {
		if ctx.Err() != nil {
			return nil
		}
		if !m.authorized() {
			m.update(generation, Expired, "room access ended; use /join @chatgpt", 0, retries)
			return nil
		}
		_ = os.Remove(launch.health) // never accept a previous process's health URL
		child, err := m.opts.start(ctx, launch)
		if err != nil {
			return fmt.Errorf("cannot launch tunnel-client; check its installation and profile with tunnel-client doctor")
		}
		m.update(generation, Starting, "waiting for tunnel health and a successful OpenAI poll", child.PID(), retries)
		reason := m.monitor(ctx, generation, launch, child, retries)
		child.Stop() // always reap this process and its stdio bridge before retrying
		if ctx.Err() != nil {
			return nil
		}
		if !m.authorized() {
			m.update(generation, Expired, "room access ended; use /join @chatgpt", 0, retries)
			return nil
		}
		if diagnostic, ok := child.(interface{ Failure() string }); ok {
			if hint := diagnostic.Failure(); hint != "" {
				return fmt.Errorf("%s; check tunnel-client doctor --profile %s, then /chatgpt restart", hint, name)
			}
		}
		if retries == 3 {
			return fmt.Errorf("%s; automatic recovery stopped after 3 retries. Check the profile with tunnel-client doctor, then /chatgpt restart", reason)
		}
		m.update(generation, Recovering, reason+"; retrying automatically", 0, retries+1)
		timer := time.NewTimer(m.opts.retryDelay * time.Duration(1<<retries))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (m *Manager) monitor(ctx context.Context, generation uint64, launch launch, child process, retries int) string {
	timer := time.NewTicker(m.opts.pollInterval)
	defer timer.Stop()
	lastHealthy := time.Now()
	wasReady := false
	for {
		select {
		case <-ctx.Done():
			return "stopped"
		case <-child.Done():
			return "tunnel process exited"
		case <-timer.C:
			if !m.authorized() {
				return "room access ended"
			}
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			ok := m.opts.probe(probe, launch, child.PID())
			cancel()
			if ctx.Err() != nil {
				return "stopped"
			}
			if ok {
				lastHealthy, wasReady = time.Now(), true
				m.update(generation, Ready, "tunnel ready; waiting for ChatGPT to join or read the room", child.PID(), retries)
			} else {
				if wasReady {
					m.update(generation, Recovering, "tunnel health check failed; allowing recovery", child.PID(), retries)
				}
				if time.Since(lastHealthy) >= m.opts.unhealthyTimeout {
					return "tunnel did not become healthy"
				}
			}
		}
	}
}

type childProcess struct {
	command *exec.Cmd
	done    chan struct{}
	once    sync.Once
	output  limitedBuffer
}

func (p *childProcess) PID() int              { return p.command.Process.Pid }
func (p *childProcess) Done() <-chan struct{} { return p.done }
func (p *childProcess) Stop()                 { p.once.Do(func() { stopProcess(p.command.Process, p.done) }) }

// Called only after Stop/Wait. Classify known failures without exposing raw
// subprocess output, profile source lines, HTTP headers, or secret values.
func (p *childProcess) Failure() string { return failureHint(string(p.output.data)) }

func failureHint(output string) string {
	value := strings.ToLower(output)
	switch {
	case strings.Contains(value, "unauthorized"), strings.Contains(value, "status=401"), strings.Contains(value, "status_code\":401"), strings.Contains(value, "invalid control plane"), strings.Contains(value, "invalid control_plane"), strings.Contains(value, "api key is malformed"):
		return "OpenAI rejected the tunnel runtime key"
	case strings.Contains(value, "forbidden"), strings.Contains(value, "status=403"), strings.Contains(value, "status_code\":403"):
		return "the runtime key lacks access to this tunnel"
	case strings.Contains(value, "flag provided but not defined"), strings.Contains(value, "unknown flag"):
		return "tunnel-client is incompatible; install a current official release"
	case strings.Contains(value, "environment variable"), strings.Contains(value, "parse config"):
		return "tunnel-client could not load the profile or a credential reference"
	}
	return ""
}

func startProcess(ctx context.Context, value launch) (process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(value.binary, "run", "--config", value.config,
		"--control-plane.tunnel-id", value.tunnelID,
		"--control-plane.poll-channel", "main", "--health.listen-addr", "127.0.0.1:0", "--health.url-file", value.health,
		"--allow-remote-ui=false", "--open-web-ui=false", "--log.http-raw-unsafe=false", "--log.file", "stdout")
	p := &childProcess{command: cmd, done: make(chan struct{})}
	cmd.Env, cmd.Stdout, cmd.Stderr = value.env, &p.output, &p.output
	cmd.WaitDelay = 2 * time.Second
	// Retain ownership even if the host crashes before its child exits. An
	// orphaned daemon must never silently compete with a newly opened room.
	if value.lock != nil {
		cmd.ExtraFiles = []*os.File{value.lock}
	}
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func probeHealth(ctx context.Context, value launch, pid int) bool {
	// Only a URL published by this newly started process in its private directory
	// is used. Reject non-loopback endpoints even if the file is altered.
	data, err := os.ReadFile(value.health)
	if err != nil || !loopbackURL(strings.TrimSpace(string(data))) {
		return false
	}
	cmd := exec.CommandContext(ctx, value.binary, "health", "--url", strings.TrimSpace(string(data)), "--pid", fmt.Sprint(pid), "--require-control-plane-poll", "--json")
	cmd.Env = value.env
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err := cmd.Run(); err != nil {
		return false
	}
	var result struct {
		Result string `json:"result"`
		Health struct {
			OK bool `json:"ok"`
		} `json:"healthz"`
		Ready struct {
			OK bool `json:"ok"`
		} `json:"readyz"`
		Poll struct {
			OK bool `json:"ok"`
		} `json:"control_plane_poll"`
	}
	return json.Unmarshal(out.data, &result) == nil && result.Result == "ok" && result.Health.OK && result.Ready.OK && result.Poll.OK
}

type limitedBuffer struct{ data []byte }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := 64*1024 - len(b.data); left > 0 {
		b.data = append(b.data, p[:min(left, n)]...)
	}
	return n, nil
}

func loopbackURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "http" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && net.ParseIP(u.Hostname()).IsLoopback() && u.Port() != ""
}

func foreignTunnel(ctx context.Context, p profile, name string) bool {
	if foreignProcess(p.path, p.id, name) {
		return true
	}
	// Also recognize the official client's managed-runtime URL file, and the
	// profile's own health listener. Never stop a process discovered this way.
	var urls []string
	files := []string{p.Health.URLFile}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if home, err := os.UserHomeDir(); err == nil {
			state = filepath.Join(home, ".local", "state")
		}
	}
	if state != "" {
		files = append(files, filepath.Join(state, "tunnel-client", "health", name+".url"))
	}
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil && len(data) < 4096 {
			urls = append(urls, strings.TrimSpace(string(data)))
		}
	}
	addr := p.Health.ListenAddress
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && port != "0" {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		urls = append(urls, "http://"+net.JoinHostPort(host, port))
	}
	client := &http.Client{Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, base := range urls {
		if !loopbackURL(base) {
			continue
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/status", nil)
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 256*1024))
			response.Body.Close()
			// A reused port or another application's /healthz is not evidence
			// that this tunnel is running. Require its identity in daemon status.
			if readErr == nil && response.StatusCode == http.StatusOK && json.Valid(data) && strings.Contains(string(data), `"`+p.id+`"`) {
				return true
			}
		}
	}
	return false
}
