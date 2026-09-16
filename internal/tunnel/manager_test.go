//go:build !windows

package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProcess struct {
	id    int
	done  chan struct{}
	once  sync.Once
	stops atomic.Int32
}

func (p *fakeProcess) PID() int              { return p.id }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) exit()                 { p.once.Do(func() { close(p.done) }) }
func (p *fakeProcess) Stop()                 { p.stops.Add(1); p.exit() }

func managerOptions(t *testing.T) Options {
	t.Helper()
	t.Setenv("MOHUDDLE_TEST_TUNNEL_KEY", "secret-never-print")
	dir := t.TempDir()
	profileSource := "control_plane:\n  tunnel_id: tunnel_0123456789abcdef0123456789abcdef\n  api_key: env:MOHUDDLE_TEST_TUNNEL_KEY\n"
	if err := os.WriteFile(filepath.Join(dir, DefaultProfile+".yaml"), []byte(profileSource), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{RuntimeDir: filepath.Join(dir, "runtime"), ProfileDir: dir, Binary: exe, Executable: exe,
		Authorized:   func() bool { return true },
		foreign:      func(context.Context, profile, string) bool { return false },
		pollInterval: time.Millisecond, unhealthyTimeout: 30 * time.Millisecond, retryDelay: time.Millisecond,
		probe: func(context.Context, launch, int) bool { return true },
	}
}

func waitStatus(t *testing.T, m *Manager, state State) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(); s.State == state {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting for %s; got %+v", state, m.Status())
	return Status{}
}

func TestManagerJoinRestartAndStop(t *testing.T) {
	opts := managerOptions(t)
	started := make(chan *fakeProcess, 8)
	var counter atomic.Int32
	opts.start = func(_ context.Context, l launch) (process, error) {
		data, err := os.ReadFile(l.config)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(data), "chatgpt serve --connection") {
			t.Error("room bridge not pinned")
		}
		p := &fakeProcess{id: int(counter.Add(1)), done: make(chan struct{})}
		started <- p
		return p, nil
	}
	m := New(opts)
	defer m.Close()
	m.Start(DefaultProfile, "/private/room.json")
	first := <-started
	waitStatus(t, m, Ready)
	for i := 0; i < 10; i++ {
		m.Start(DefaultProfile, "/private/room.json")
	}
	if counter.Load() != 1 {
		t.Fatal("repeated joins launched duplicates")
	}
	m.Restart(DefaultProfile, "/private/room.json")
	second := <-started
	if first.stops.Load() != 1 {
		t.Fatal("replacement started before old child stopped")
	}
	waitStatus(t, m, Ready)
	m.Stop()
	m.Close()
	if second.stops.Load() != 1 || m.Status().State != Stopped || counter.Load() != 2 {
		t.Fatalf("stop restarted the tunnel: %+v", m.Status())
	}
}

func TestManagerCrashRecoveryIsBounded(t *testing.T) {
	opts := managerOptions(t)
	var count atomic.Int32
	opts.start = func(context.Context, launch) (process, error) {
		p := &fakeProcess{id: int(count.Add(1)), done: make(chan struct{})}
		p.exit()
		return p, nil
	}
	m := New(opts)
	defer m.Close()
	m.Start(DefaultProfile, "/private/room.json")
	s := waitStatus(t, m, Failed)
	if count.Load() != 4 || s.Restarts != 3 || !strings.Contains(s.Detail, "3 retries") {
		t.Fatalf("unbounded retries: %d %+v", count.Load(), s)
	}
	// A fresh, explicit user action is required to retry after exhaustion.
	m.Start(DefaultProfile, "/private/room.json")
	waitStatus(t, m, Failed)
	if count.Load() != 8 {
		t.Fatal("explicit retry did not reset the bounded attempt budget")
	}
}

func TestManagerRequiresHealthAndStopsOnGrantExpiry(t *testing.T) {
	opts := managerOptions(t)
	var authorized, healthy atomic.Bool
	authorized.Store(true)
	opts.Authorized = authorized.Load
	opts.probe = func(context.Context, launch, int) bool { return healthy.Load() }
	opts.unhealthyTimeout = time.Second
	child := &fakeProcess{id: 1, done: make(chan struct{})}
	opts.start = func(context.Context, launch) (process, error) { return child, nil }
	m := New(opts)
	defer m.Close()
	m.Start(DefaultProfile, "/private/room.json")
	time.Sleep(10 * time.Millisecond)
	if m.Status().State == Ready {
		t.Fatal("a started PID was reported ready without health")
	}
	healthy.Store(true)
	waitStatus(t, m, Ready)
	authorized.Store(false)
	waitStatus(t, m, Expired)
	if child.stops.Load() != 1 {
		t.Fatal("expired grant did not stop the child")
	}
}

func TestManagerStopCancelsPendingStartAndBackoff(t *testing.T) {
	for _, phase := range []string{"probe", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			opts := managerOptions(t)
			entered := make(chan struct{})
			var starts atomic.Int32
			opts.start = func(context.Context, launch) (process, error) {
				starts.Add(1)
				p := &fakeProcess{id: 1, done: make(chan struct{})}
				p.exit()
				return p, nil
			}
			if phase == "probe" {
				opts.ProbeRoom = func(ctx context.Context, _ string) error { close(entered); <-ctx.Done(); return ctx.Err() }
			} else {
				opts.retryDelay = time.Hour
			}
			m := New(opts)
			defer m.Close()
			m.Start(DefaultProfile, "/private/room.json")
			if phase == "probe" {
				<-entered
			} else {
				waitStatus(t, m, Recovering)
			}
			m.Stop()
			m.Close()
			if m.Status().State != Stopped || starts.Load() > 1 {
				t.Fatalf("stop lost race: %+v", m.Status())
			}
		})
	}
}

func TestManagerDoesNotTakeOverAnotherRoomOrExternalTunnel(t *testing.T) {
	opts := managerOptions(t)
	var count atomic.Int32
	opts.start = func(context.Context, launch) (process, error) {
		return &fakeProcess{id: int(count.Add(1)), done: make(chan struct{})}, nil
	}
	first := New(opts)
	defer first.Close()
	first.Start(DefaultProfile, "/room-one.json")
	waitStatus(t, first, Ready)
	second := New(opts)
	defer second.Close()
	second.Start(DefaultProfile, "/room-two.json")
	if status := waitStatus(t, second, Failed); !strings.Contains(status.Detail, "another MoHuddle room") {
		t.Fatal(status)
	}
	if count.Load() != 1 || first.Status().State != Ready {
		t.Fatal("foreign room was interrupted")
	}
	first.Close()
	opts.foreign = func(context.Context, profile, string) bool { return true }
	third := New(opts)
	defer third.Close()
	third.Start(DefaultProfile, "/room-three.json")
	if status := waitStatus(t, third, Failed); !strings.Contains(status.Detail, "separately started") {
		t.Fatal(status)
	}
	if count.Load() != 1 {
		t.Fatal("competing external tunnel launched")
	}
}

func TestUnhealthyTunnelRestartsAndRecovers(t *testing.T) {
	opts := managerOptions(t)
	var count atomic.Int32
	opts.start = func(context.Context, launch) (process, error) {
		return &fakeProcess{id: int(count.Add(1)), done: make(chan struct{})}, nil
	}
	opts.probe = func(_ context.Context, _ launch, pid int) bool { return pid > 1 }
	m := New(opts)
	defer m.Close()
	m.Start(DefaultProfile, "/room.json")
	s := waitStatus(t, m, Ready)
	if count.Load() != 2 || s.Restarts != 1 {
		t.Fatalf("health recovery failed: %+v", s)
	}
}
