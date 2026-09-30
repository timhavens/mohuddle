package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/chatgpt"
	"github.com/timhavens/mohuddle/internal/research"
	"github.com/timhavens/mohuddle/internal/room"
	"github.com/timhavens/mohuddle/internal/roommanager"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/tunnel"
)

type managedRuntime struct {
	orchestrator *room.Orchestrator
	api          *apiRuntime
	lock         *store.RoomLock
}

type managedRooms struct {
	shared *room.SharedCapacity
	*store.Store
	mu          sync.Mutex
	opts        options
	workspace   string
	preferences *appsettings.Store
	launch      map[chat.Participant]chat.AgentSettings
	rooms       map[string]*managedRuntime
	router      *roommanager.Router
	server      *api.Server
	tunnel      *tunnel.Manager
	stop        chan struct{}
	done        chan struct{}
	overview    []string
	closed      bool
}

func newManagedRooms(s *store.Store, workspace string, opts options, prefs *appsettings.Store, launch map[chat.Participant]chat.AgentSettings) (*managedRooms, error) {
	m := &managedRooms{shared: room.NewSharedCapacity(), Store: s, workspace: workspace, opts: opts, preferences: prefs, launch: launch, rooms: map[string]*managedRuntime{}, stop: make(chan struct{}), done: make(chan struct{})}
	router, err := roommanager.New(s, workspace, func(id string) (*api.Service, error) {
		runtime, err := m.Open(id)
		if err != nil {
			return nil, err
		}
		if runtime.api.service == nil {
			return nil, fmt.Errorf("private local API is unavailable")
		}
		return runtime.api.service, nil
	})
	if err != nil {
		return nil, err
	}
	m.router = router
	router.ConfigureOwnership(func(id string) bool { m.mu.Lock(); defer m.mu.Unlock(); return m.rooms[id] != nil })
	go m.monitor()
	return m, nil
}

func (m *managedRooms) Open(id string) (*managedRuntime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("room manager is shutting down")
	}
	if runtime := m.rooms[id]; runtime != nil {
		return runtime, nil
	}
	state, err := m.LoadRoom(id)
	if err != nil {
		return nil, err
	}
	messages, err := m.LoadMessages(id)
	if err != nil {
		return nil, err
	}
	lock, err := m.AcquireRoomLock(id)
	if err != nil {
		return nil, fmt.Errorf("this room is already open in another MoHuddle process")
	}
	ok := false
	defer func() {
		if !ok {
			_ = lock.Release()
		}
	}()
	reconcileWorkerRoster(&state, m.preferences.WorkerCounts())
	agents, err := buildAgents(m.opts, state, m.preferences, m.launch)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("%s", noProviderGuidance)
	}
	for _, p := range state.PresentAgents() {
		if effectiveSettings(m.preferences, state, m.launch, p).Permissions == chat.PermissionFull && !m.preferences.FullAccessAcknowledged() {
			for _, a := range agents {
				_ = a.Close()
			}
			return nil, fmt.Errorf("saved full access requires acknowledgement in /settings")
		}
	}
	o, err := room.New(state, messages, m.Store, agents...)
	if err != nil {
		for _, a := range agents {
			_ = a.Close()
		}
		return nil, err
	}
	// Primary events must always drain, even while another room is displayed.
	go func() {
		for range o.Events() {
		}
	}()
	if err := o.ConfigureSharedCapacity(m.shared); err != nil {
		_ = o.Close()
		return nil, err
	}
	o.ConfigureResearch(research.New(filepath.Join(m.Root(), "research_audit.jsonl")))
	if err := o.Configure(m.preferences, m.launch); err != nil {
		_ = o.Close()
		return nil, err
	}
	o.RefreshSharedCapacity()
	o.ConfigureTemporaryAgents(newTemporaryAgentFactory(m.opts, agents, m.preferences, state, m.launch))
	roomOpts := m.opts
	if len(m.rooms) > 0 {
		roomOpts.apiSocket = ""
		roomOpts.federationListen = ""
		roomOpts.remoteListen = ""
	}
	runtime, err := startAPIServers(roomOpts, m.Store, o, id)
	if err != nil {
		_ = o.Close()
		return nil, err
	}
	if runtime.service != nil {
		if err := runtime.service.SetChatGPTLimits(m.preferences.ChatGPTLimits(filepath.Join(m.Root(), "chatgpt-"+id+".json"))); err != nil {
			_ = runtime.Close()
			_ = o.Close()
			return nil, err
		}
	}
	value := &managedRuntime{o, runtime, lock}
	m.rooms[id] = value
	ok = true
	return value, nil
}

func (m *managedRooms) StartGateway(initial *managedRuntime) error {
	if initial.api.service == nil {
		return nil
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(m.Root()))
	if err != nil {
		return err
	}
	gateway, err := api.NewService(*credentials, initial.orchestrator)
	if err != nil {
		return err
	}
	gateway.ConfigureManager(m.router)
	server, err := api.StartLocal(api.DefaultSocketPath(m.Root(), "manager"), gateway, api.NewAuditLog(filepath.Join(m.Root(), "api_audit.jsonl")))
	if err != nil {
		return fmt.Errorf("start room manager: %w", err)
	}
	m.server = server
	journal, readErr := os.ReadFile(filepath.Join(m.Root(), "workspace_activity.json"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if err := m.shared.ConfigureWorkspaceJournal(journal, func(data []byte) error { return m.WritePrivateState("workspace_activity.json", data) }); err != nil {
		return err
	}
	executable, _ := os.Executable()
	shared, _ := store.DefaultStateDir()
	m.tunnel = tunnel.New(tunnel.Options{RuntimeDir: filepath.Join(shared, "tunnels"), Executable: executable, Authorized: m.router.Enabled, ProbeRoom: func(ctx context.Context, path string) error {
		bridge, err := chatgpt.NewFromFile(path)
		if err != nil {
			return err
		}
		return bridge.Doctor(ctx)
	}, ServeMCP: func(ctx context.Context, path, socket string) (io.Closer, error) {
		bridge, err := chatgpt.NewFromFile(path)
		if err != nil {
			return nil, err
		}
		return bridge.ServePrivate(ctx, socket)
	}})
	return nil
}

func (m *managedRooms) EnableRoom(id string, ttl time.Duration) (string, error) {
	if m.server == nil {
		return "", fmt.Errorf("ChatGPT requires the private local API")
	}
	if err := m.router.SetRoomEnabled(id, true); err != nil {
		return "", err
	}
	return m.router.Enable(m.server.Addr(), ttl)
}
func (m *managedRooms) DisableRoom(id string) error { return m.router.SetRoomEnabled(id, false) }
func (m *managedRooms) ConnectionPath() string      { return m.router.Path() }

func (m *managedRooms) HasActiveWork() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, runtime := range m.rooms {
		if runtime.orchestrator.HasActiveWork() {
			return true
		}
	}
	return false
}

func (m *managedRooms) CloseRoom(selector string) error {
	state, err := m.ResolveRoom(selector)
	if err != nil {
		return err
	}
	if err := m.router.SetRoomEnabled(state.ID, false); err != nil {
		return err
	}
	m.mu.Lock()
	runtime := m.rooms[state.ID]
	delete(m.rooms, state.ID)
	m.mu.Unlock()
	if runtime == nil {
		return nil
	}
	return closeManagedRuntime(runtime)
}

func closeManagedRuntime(runtime *managedRuntime) error {
	first := runtime.api.Close()
	if err := runtime.orchestrator.Close(); first == nil {
		first = err
	}
	if err := runtime.lock.Release(); first == nil {
		first = err
	}
	return first
}

func (m *managedRooms) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	list := m.rooms
	m.rooms = map[string]*managedRuntime{}
	m.mu.Unlock()
	close(m.stop)
	<-m.done
	if m.tunnel != nil {
		m.tunnel.Close()
	}
	m.router.Close()
	var first error
	if m.server != nil {
		first = m.server.Close()
	}
	for _, runtime := range list {
		if err := closeManagedRuntime(runtime); first == nil {
			first = err
		}
	}
	return first
}

func (m *managedRooms) monitor() {
	defer close(m.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.refreshOverview(now)
		}
	}
}

func (m *managedRooms) refreshOverview(now time.Time) {
	m.mu.Lock()
	list := make(map[string]*managedRuntime, len(m.rooms))
	for id, r := range m.rooms {
		list[id] = r
	}
	m.mu.Unlock()
	names, err := m.RoomNames()
	if err != nil {
		return
	}
	lines := []string{}
	states, _ := m.ListRooms()
	for _, state := range states {
		if list[state.ID] == nil {
			lines = append(lines, names[state.ID]+": saved · "+state.Workspace)
		}
	}
	for id, runtime := range list {
		status := "idle"
		if runtime.orchestrator.HasActiveWork() {
			status = "work in progress"
		}
		if runtime.api.service != nil {
			if v, err := runtime.api.service.CoordinationStatus(now); err != nil {
				status = "monitor error: " + err.Error()
			} else if v != nil {
				status = v.Summary
			}
		}
		lines = append(lines, names[id]+": "+status)
	}
	sort.Strings(lines)
	lines = append([]string{"Rooms · updates every 5 seconds · /resume room2 to view a room", "Switching views keeps accepted work running. /quit stops all rooms.", ""}, lines...)
	m.mu.Lock()
	m.overview = lines
	m.mu.Unlock()
}
func (m *managedRooms) Overview() []string {
	m.mu.Lock()
	empty := len(m.overview) == 0
	m.mu.Unlock()
	if empty {
		m.refreshOverview(time.Now())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.overview...)
}

func (m *managedRooms) ReloadWorkers() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rooms {
		if r.orchestrator.HasActiveWork() {
			return fmt.Errorf("worker topology cannot change while another room has active work")
		}
	}
	type replacement struct {
		runtime *managedRuntime
		state   chat.Room
		agents  []agent.Agent
	}
	prepared := []replacement{}
	defer func() {
		for _, item := range prepared {
			for _, a := range item.agents {
				_ = a.Close()
			}
		}
	}()
	// Validate every provider runtime before replacing any room's agents.
	for _, r := range m.rooms {
		state, _ := r.orchestrator.Snapshot()
		reconcileWorkerRoster(&state, m.preferences.WorkerCounts())
		agents, err := buildAgents(m.opts, state, m.preferences, m.launch)
		if err != nil {
			return err
		}
		prepared = append(prepared, replacement{r, state, agents})
	}
	for i, item := range prepared {
		prepared[i].agents = nil // ReplaceAgents takes ownership, including on failure.
		if err := item.runtime.orchestrator.ReplaceAgents(item.agents, item.state.Members); err != nil {
			return err
		}
		if err := item.runtime.orchestrator.Configure(m.preferences, m.launch); err != nil {
			return err
		}
		item.runtime.orchestrator.RefreshSharedCapacity()
		item.runtime.orchestrator.ConfigureTemporaryAgents(newTemporaryAgentFactory(m.opts, item.agents, m.preferences, item.state, m.launch))
	}
	return nil
}

func (m *managedRooms) SetWorkerCounts(counts map[chat.Participant]int) error {
	return m.router.WithDispatchPaused(func() error {
		if m.HasActiveWork() {
			return fmt.Errorf("worker topology cannot change while any room has active work")
		}
		old := m.preferences.WorkerCounts()
		if err := m.preferences.SetWorkerCounts(counts); err != nil {
			return err
		}
		if err := m.ReloadWorkers(); err != nil {
			if restore := m.preferences.SetWorkerCounts(old); restore != nil {
				return errors.Join(err, fmt.Errorf("restore worker defaults: %w", restore))
			}
			if restore := m.ReloadWorkers(); restore != nil {
				return errors.Join(err, fmt.Errorf("restore room workers: %w; restart idle rooms to reconcile saved defaults", restore))
			}
			return err
		}
		return nil
	})
}
