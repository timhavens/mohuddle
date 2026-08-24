package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/agent/agy"
	"github.com/timhavens/mohuddle/internal/agent/claude"
	"github.com/timhavens/mohuddle/internal/agent/codex"
	"github.com/timhavens/mohuddle/internal/agent/copilot"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	remoteaccess "github.com/timhavens/mohuddle/internal/remote"
	"github.com/timhavens/mohuddle/internal/remote/device"
	"github.com/timhavens/mohuddle/internal/remoteui"
	"github.com/timhavens/mohuddle/internal/research"
	"github.com/timhavens/mohuddle/internal/room"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/speech"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/ui"
)

type options struct {
	workspace          string
	roomID             string
	newRoom            bool
	showVersion        bool
	maxWaves           int
	maxTurns           int
	parseErr           error
	codexBinary        string
	claudeBinary       string
	agyBinary          string
	copilotBinary      string
	codexModel         string
	claudeModel        string
	agyModel           string
	copilotModel       string
	codexEffort        string
	claudeEffort       string
	agyEffort          string
	copilotEffort      string
	codexPermissions   string
	claudePermissions  string
	agyPermissions     string
	copilotPermissions string
	stateDir           string
	configPath         string
	apiSocket          string
	noAPI              bool
	federationListen   string
	remoteListen       string
	remoteOrigin       string
	remoteTLSCert      string
	remoteTLSKey       string
	explicitBinaries   map[chat.Participant]bool
}

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mohuddle:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "pair" {
		return runPairCommand(os.Args[2:])
	}
	opts := parseFlags()
	if opts.parseErr != nil {
		if errors.Is(opts.parseErr, flag.ErrHelp) {
			return nil
		}
		return opts.parseErr
	}
	if opts.showVersion {
		fmt.Println("mohuddle " + version)
		return nil
	}
	workspace, err := access.CanonicalDirectory(opts.workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	roomStore, err := store.New(opts.stateDir)
	if err != nil {
		return err
	}
	preferences, err := appsettings.Open(opts.configPath)
	if err != nil {
		return err
	}
	launch, err := launchSettings(opts)
	if err != nil {
		return err
	}
	for participant, value := range launch {
		if value.Permissions == chat.PermissionFull && !preferences.FullAccessAcknowledged() {
			return fmt.Errorf("--%s-permissions=full requires a one-time acknowledgement in /settings", participant)
		}
	}

	nextRoomID := opts.roomID
	forceNew := opts.newRoom
	for {
		roomState, messages, err := selectRoom(roomStore, workspace, nextRoomID, forceNew, opts.maxWaves)
		if err != nil {
			return err
		}
		reconcileWorkerRoster(&roomState, preferences.WorkerCounts())
		agents, err := buildAgents(opts, roomState, preferences, launch)
		if err != nil {
			return err
		}
		for _, participant := range roomState.PresentAgents() {
			value := effectiveSettings(preferences, roomState, launch, participant)
			if value.Permissions == chat.PermissionFull && !preferences.FullAccessAcknowledged() {
				return fmt.Errorf("saved full access requires a one-time acknowledgement in /settings")
			}
		}
		orchestrator, err := room.New(roomState, messages, roomStore, agents...)
		if err != nil {
			return err
		}
		orchestrator.ConfigureResearch(research.New(filepath.Join(roomStore.Root(), "research_audit.jsonl")))
		if err := orchestrator.Configure(preferences, launch); err != nil {
			return err
		}
		orchestrator.ConfigureTemporaryAgents(newTemporaryAgentFactory(opts, agents, preferences, roomState, launch))
		apiRuntime, err := startAPIServers(opts, roomStore, orchestrator, roomState.ID)
		if err != nil {
			_ = orchestrator.Close()
			return err
		}
		speechConfig := preferences.SpeechSettings()
		speechService := speech.New(speechConfig, speech.NewProvider(speechConfig), preferences.SetSpeechSettings)
		model := ui.New(orchestrator, roomStore, speechService)
		model.ConfigureRemote(apiRuntime.devices, apiRuntime.remoteOrigin(), apiRuntime.audit)
		program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
		final, runErr := program.Run()
		speechCloseErr := speechService.Close()
		apiCloseErr := apiRuntime.Close()
		closeErr := orchestrator.Close()
		if runErr != nil {
			return runErr
		}
		if speechCloseErr != nil {
			return speechCloseErr
		}
		if apiCloseErr != nil {
			return apiCloseErr
		}
		if closeErr != nil {
			return closeErr
		}
		finalModel, ok := final.(ui.Model)
		if !ok {
			return nil
		}
		action := finalModel.Action()
		if !action.NewRoom && action.ResumeID == "" {
			return nil
		}
		nextRoomID = action.ResumeID
		forceNew = action.NewRoom
	}
}

func parseFlags() options {
	value, err := parseOptions(os.Args[1:])
	value.parseErr = err
	return value
}

func parseOptions(args []string) (options, error) {
	var value options
	flags := flag.NewFlagSet("mohuddle", flag.ContinueOnError)
	flags.StringVar(&value.workspace, "workspace", ".", "initial workspace directory")
	flags.StringVar(&value.roomID, "room", "", "resume a saved room by ID")
	flags.BoolVar(&value.newRoom, "new", false, "start a new room instead of resuming the latest room for this workspace")
	flags.BoolVar(&value.showVersion, "version", false, "print the MoHuddle version and exit")
	flags.IntVar(&value.maxWaves, "max-waves", 3, "deprecated compatibility value; moderated rounds are structurally bounded")
	flags.IntVar(&value.maxTurns, "max-turns", 0, "deprecated compatibility alias for --max-waves")
	flags.StringVar(&value.codexBinary, "codex-binary", "codex", "Codex CLI binary")
	flags.StringVar(&value.claudeBinary, "claude-binary", "claude", "Claude Code CLI binary")
	flags.StringVar(&value.agyBinary, "agy-binary", "agy", "Google Antigravity CLI binary")
	flags.StringVar(&value.copilotBinary, "copilot-binary", "copilot", "GitHub Copilot CLI binary")
	flags.StringVar(&value.codexModel, "codex-model", "", "Codex model override (default: CLI configuration)")
	flags.StringVar(&value.claudeModel, "claude-model", "", "Claude model override (default: CLI configuration)")
	flags.StringVar(&value.agyModel, "agy-model", "", "AGY model override (default: CLI configuration)")
	flags.StringVar(&value.copilotModel, "copilot-model", "", "Copilot model override (default: auto)")
	flags.StringVar(&value.codexEffort, "codex-effort", "", "Codex effort override")
	flags.StringVar(&value.claudeEffort, "claude-effort", "", "Claude effort override")
	flags.StringVar(&value.agyEffort, "agy-effort", "", "AGY effort override")
	flags.StringVar(&value.copilotEffort, "copilot-effort", "", "Copilot effort override")
	flags.StringVar(&value.codexPermissions, "codex-permissions", "", "Codex permissions: read-only, workspace, or full")
	flags.StringVar(&value.claudePermissions, "claude-permissions", "", "Claude permissions: read-only, workspace, or full")
	flags.StringVar(&value.agyPermissions, "agy-permissions", "", "AGY permissions: read-only, workspace, or full")
	flags.StringVar(&value.copilotPermissions, "copilot-permissions", "", "Copilot permissions: read-only, workspace, or full")
	flags.StringVar(&value.stateDir, "state-dir", "", "room state directory")
	flags.StringVar(&value.configPath, "config", "", "personal settings file")
	flags.StringVar(&value.apiSocket, "api-socket", "", "local API Unix socket path (default: room-specific path in the state directory)")
	flags.BoolVar(&value.noAPI, "no-api", false, "disable the local command-and-event API")
	flags.StringVar(&value.federationListen, "federation-listen", "", "explicit TLS federation listen address (disabled by default)")
	flags.StringVar(&value.remoteListen, "remote-listen", "", "explicit phone web gateway listen address (disabled by default)")
	flags.StringVar(&value.remoteOrigin, "remote-origin", "", "exact browser origin for the phone web gateway")
	flags.StringVar(&value.remoteTLSCert, "remote-tls-cert", "", "TLS certificate for the phone web gateway")
	flags.StringVar(&value.remoteTLSKey, "remote-tls-key", "", "TLS private key for the phone web gateway")
	if err := flags.Parse(args); err != nil {
		return value, err
	}
	seen := make(map[string]bool)
	flags.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if seen["max-waves"] && seen["max-turns"] {
		return value, fmt.Errorf("--max-waves and deprecated --max-turns cannot be used together")
	}
	if seen["max-turns"] {
		value.maxWaves = value.maxTurns
	}
	if value.noAPI && value.apiSocket != "" {
		return value, fmt.Errorf("--no-api and --api-socket cannot be used together")
	}
	if value.remoteListen == "" && (value.remoteOrigin != "" || value.remoteTLSCert != "" || value.remoteTLSKey != "") {
		return value, fmt.Errorf("remote origin and TLS options require --remote-listen")
	}
	if (value.remoteTLSCert == "") != (value.remoteTLSKey == "") {
		return value, fmt.Errorf("--remote-tls-cert and --remote-tls-key must be used together")
	}
	value.explicitBinaries = map[chat.Participant]bool{
		chat.Codex: seen["codex-binary"], chat.Claude: seen["claude-binary"],
		chat.Agy: seen["agy-binary"], chat.Copilot: seen["copilot-binary"],
	}
	if value.maxWaves < 1 {
		return value, fmt.Errorf("--max-waves must be at least 1")
	}
	return value, nil
}

type apiRuntime struct {
	servers []*api.Server
	remote  *remoteaccess.Gateway
	devices *device.Store
	audit   *api.AuditLog
}

func (r *apiRuntime) remoteOrigin() string {
	if r == nil || r.remote == nil {
		return ""
	}
	return r.remote.Origin()
}

func (r *apiRuntime) Close() error {
	if r == nil {
		return nil
	}
	var first error
	if r.remote != nil {
		if err := r.remote.Close(); err != nil {
			first = err
		}
	}
	if r.devices != nil {
		r.devices.Close()
	}
	for index := len(r.servers) - 1; index >= 0; index-- {
		if err := r.servers[index].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func startAPIServers(opts options, roomStore *store.Store, orchestrator *room.Orchestrator, roomID string) (*apiRuntime, error) {
	runtime := &apiRuntime{}
	if opts.noAPI && opts.federationListen == "" && opts.remoteListen == "" {
		return runtime, nil
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(roomStore.Root()))
	if err != nil {
		return nil, err
	}
	service, err := api.NewService(*credentials, orchestrator)
	if err != nil {
		return nil, err
	}
	runtime.audit = api.NewAuditLog(filepath.Join(roomStore.Root(), "api_audit.jsonl"))
	runtime.servers = make([]*api.Server, 0, 2)
	closeRuntime := func() { _ = runtime.Close() }
	if !opts.noAPI {
		if !api.LocalTransportSupported() {
			if opts.apiSocket != "" {
				return nil, fmt.Errorf("local API transport is unavailable on this platform")
			}
		} else {
			path := opts.apiSocket
			if path == "" {
				path = api.DefaultSocketPath(roomStore.Root(), roomID)
			}
			server, err := api.StartLocal(path, service, runtime.audit)
			if err != nil {
				return nil, fmt.Errorf("start local API: %w", err)
			}
			runtime.servers = append(runtime.servers, server)
		}
	}
	if opts.federationListen != "" {
		identity, err := api.LoadOrCreateFederationIdentity(api.FederationIdentityPath(roomStore.Root()), credentials.InstanceID)
		if err != nil {
			closeRuntime()
			return nil, err
		}
		pairings, err := api.LoadPairingStore(api.FederationPairingsPath(roomStore.Root()), credentials.InstanceID)
		if err != nil {
			closeRuntime()
			return nil, err
		}
		server, err := api.StartFederation(opts.federationListen, service, runtime.audit, identity, pairings)
		if err != nil {
			closeRuntime()
			return nil, fmt.Errorf("start federation API: %w", err)
		}
		runtime.servers = append(runtime.servers, server)
	}
	if opts.remoteListen != "" {
		runtime.devices, err = device.Open(filepath.Join(roomStore.Root(), "remote_devices.json"))
		if err != nil {
			closeRuntime()
			return nil, err
		}
		runtime.remote, err = remoteaccess.Start(remoteaccess.Config{
			ListenAddress: opts.remoteListen, Origin: opts.remoteOrigin,
			TLSCertFile: opts.remoteTLSCert, TLSKeyFile: opts.remoteTLSKey,
			RoomID: roomID, Service: service, Devices: runtime.devices,
			Audit: runtime.audit, Assets: remoteui.FS(),
		})
		if err != nil {
			closeRuntime()
			return nil, fmt.Errorf("start remote phone gateway: %w", err)
		}
	}
	return runtime, nil
}

func launchSettings(opts options) (map[chat.Participant]chat.AgentSettings, error) {
	result := make(map[chat.Participant]chat.AgentSettings, len(chat.Agents()))
	values := []struct {
		participant                chat.Participant
		model, effort, permissions string
	}{
		{chat.Codex, opts.codexModel, opts.codexEffort, opts.codexPermissions},
		{chat.Claude, opts.claudeModel, opts.claudeEffort, opts.claudePermissions},
		{chat.Agy, opts.agyModel, opts.agyEffort, opts.agyPermissions},
		{chat.Copilot, opts.copilotModel, opts.copilotEffort, opts.copilotPermissions},
	}
	for _, item := range values {
		if item.model == "" && item.effort == "" && item.permissions == "" {
			continue
		}
		value := chat.AgentSettings{Model: item.model, Effort: item.effort}
		if item.permissions != "" {
			value.Permissions = chat.PermissionProfile(strings.ToLower(item.permissions))
			if !value.Permissions.Valid() {
				return nil, fmt.Errorf("invalid --%s-permissions value %q", item.participant, item.permissions)
			}
		}
		if value.Effort != "" {
			candidate := value.WithDefaults()
			if err := appsettings.ValidateFor(item.participant, candidate); err != nil {
				return nil, fmt.Errorf("invalid --%s-effort: %w", item.participant, err)
			}
		}
		result[item.participant] = value
	}
	return result, nil
}

func mergeSettings(base, override chat.AgentSettings) chat.AgentSettings {
	if override.Model != "" {
		base.Model = override.Model
	}
	if override.Effort != "" {
		base.Effort = override.Effort
	}
	if override.Permissions.Valid() {
		base.Permissions = override.Permissions
	}
	return base.WithDefaults()
}

func effectiveSettings(preferences *appsettings.Store, roomState chat.Room, launch map[chat.Participant]chat.AgentSettings, participant chat.Participant) chat.AgentSettings {
	value := preferences.Effective(roomState, participant)
	override, ok := launch[participant]
	if !ok && participant.IsAuxiliary() {
		override, ok = launch[participant.Provider()]
		// Provider-wide command-line model and effort choices are useful for new
		// worker instances. Permission overrides are intentionally not inherited:
		// auxiliary workers start read-only unless their own saved settings elevate
		// them explicitly.
		override.Permissions = ""
	}
	if ok {
		value = mergeSettings(value, override)
	}
	return value
}

func reconcileWorkerRoster(roomState *chat.Room, counts map[chat.Participant]int) {
	if roomState.Members == nil {
		roomState.Members = map[chat.Participant]bool{chat.Codex: true, chat.Claude: true}
	}
	if roomState.Sessions == nil {
		roomState.Sessions = make(map[chat.Participant]chat.AgentSession)
	}
	for _, participant := range appsettings.WorkerParticipants(counts) {
		if !participant.IsAuxiliary() {
			continue
		}
		_, memberKnown := roomState.Members[participant]
		_, sessionKnown := roomState.Sessions[participant]
		// A worker is new only when neither membership nor session state has ever
		// been persisted. Current leave state is an explicit false membership, but
		// older room snapshots can have a missing membership key and a retained
		// session; treating that worker as new would silently rejoin it.
		if !memberKnown && !sessionKnown {
			roomState.Members[participant] = true
		}
		if !sessionKnown {
			roomState.Sessions[participant] = chat.AgentSession{}
		}
	}
	// Deconfigured workers keep their membership bit, session, cursor, and
	// exact settings as dormant state. They are absent from the runtime agent
	// map, so they cannot be scheduled; re-enabling the same stable identity
	// restores its prior present/away choice and provider session.
}

func buildAgents(opts options, roomState chat.Room, preferences *appsettings.Store, launch map[chat.Participant]chat.AgentSettings) ([]agent.Agent, error) {
	binaries := map[chat.Participant]string{
		chat.Codex: opts.codexBinary, chat.Claude: opts.claudeBinary,
		chat.Agy: opts.agyBinary, chat.Copilot: opts.copilotBinary,
	}
	participants := appsettings.WorkerParticipants(preferences.WorkerCounts())
	type runtimeState struct {
		binary    string
		available bool
	}
	runtimes := make(map[chat.Participant]runtimeState, len(binaries))
	for _, provider := range chat.Agents() {
		binary := binaries[provider]
		if _, err := exec.LookPath(binary); err != nil {
			if opts.explicitBinaries[provider] {
				return nil, fmt.Errorf("configured %s binary %q is unavailable: %w", provider, binary, err)
			}
			continue
		}
		present := false
		for _, participant := range participants {
			if participant.Provider() == provider && roomState.Present(participant) {
				present = true
				break
			}
		}
		if present {
			switch provider {
			case chat.Codex:
				if err := verifyRuntime(binary, "login", "status"); err != nil {
					if opts.explicitBinaries[provider] {
						return nil, fmt.Errorf("configured Codex runtime is unavailable or not authenticated: %w", err)
					}
					continue
				}
			case chat.Claude:
				if err := verifyRuntime(binary, "auth", "status"); err != nil {
					if opts.explicitBinaries[provider] {
						return nil, fmt.Errorf("configured Claude runtime is unavailable or not authenticated: %w", err)
					}
					continue
				}
			}
		}
		runtimes[provider] = runtimeState{binary: binary, available: true}
	}

	result := make([]agent.Agent, 0, len(participants))
	for _, participant := range participants {
		provider := participant.Provider()
		runtime := runtimes[provider]
		if !runtime.available {
			continue
		}
		settings := effectiveSettings(preferences, roomState, launch, participant)
		sessionID := roomState.Sessions[participant].ID
		var base agent.Agent
		switch provider {
		case chat.Codex:
			base = codex.New(codex.Config{Binary: runtime.binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID})
		case chat.Claude:
			base = claude.New(claude.Config{Binary: runtime.binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID})
		case chat.Agy:
			base = agy.New(agy.Config{Binary: runtime.binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID})
		case chat.Copilot:
			base = copilot.New(copilot.Config{Binary: runtime.binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID})
		}
		instance, err := agent.WithParticipant(base, participant)
		if err != nil {
			for _, created := range result {
				_ = created.Close()
			}
			return nil, err
		}
		result = append(result, instance)
	}
	return result, nil
}

type temporaryAgentFactory struct {
	binaries  map[chat.Participant]string
	settings  map[chat.Participant]chat.AgentSettings
	providers []chat.Participant
}

func newTemporaryAgentFactory(opts options, configured []agent.Agent, preferences *appsettings.Store, roomState chat.Room, launch map[chat.Participant]chat.AgentSettings) *temporaryAgentFactory {
	factory := &temporaryAgentFactory{
		binaries: map[chat.Participant]string{
			chat.Codex: opts.codexBinary, chat.Claude: opts.claudeBinary,
			chat.Agy: opts.agyBinary, chat.Copilot: opts.copilotBinary,
		},
		settings: make(map[chat.Participant]chat.AgentSettings),
	}
	seen := make(map[chat.Participant]bool)
	for _, configuredAgent := range configured {
		provider := configuredAgent.Participant().Provider()
		if seen[provider] {
			continue
		}
		seen[provider] = true
		factory.providers = append(factory.providers, provider)
		settings := effectiveSettings(preferences, roomState, launch, provider)
		settings.Permissions = chat.PermissionReadOnly
		factory.settings[provider] = settings
	}
	return factory
}

func (f *temporaryAgentFactory) Providers() []chat.Participant {
	if f == nil {
		return nil
	}
	return append([]chat.Participant(nil), f.providers...)
}

func (f *temporaryAgentFactory) Create(provider, participant chat.Participant) (agent.Agent, error) {
	if f == nil || participant.Provider() != provider || !provider.IsPrimaryAgent() {
		return nil, fmt.Errorf("invalid temporary responder %q for provider %q", participant, provider)
	}
	settings, ok := f.settings[provider]
	if !ok {
		return nil, fmt.Errorf("provider %s is unavailable for temporary responders", provider)
	}
	settings.Permissions = chat.PermissionReadOnly
	var base agent.Agent
	switch provider {
	case chat.Codex:
		base = codex.New(codex.Config{Binary: f.binaries[provider], Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions})
	case chat.Claude:
		base = claude.New(claude.Config{Binary: f.binaries[provider], Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions})
	case chat.Agy:
		base = agy.New(agy.Config{Binary: f.binaries[provider], Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions})
	case chat.Copilot:
		base = copilot.New(copilot.Config{Binary: f.binaries[provider], Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions})
	default:
		return nil, fmt.Errorf("provider %s is unavailable for temporary responders", provider)
	}
	return agent.WithParticipant(base, participant)
}

func selectRoom(roomStore *store.Store, workspace, roomID string, forceNew bool, maxWaves int) (chat.Room, []chat.Message, error) {
	if roomID != "" && forceNew {
		return chat.Room{}, nil, fmt.Errorf("--room and --new cannot be used together")
	}
	if roomID != "" {
		roomState, err := roomStore.LoadRoom(roomID)
		if err != nil {
			return chat.Room{}, nil, fmt.Errorf("load room: %w", err)
		}
		messages, err := roomStore.LoadMessages(roomState.ID)
		return roomState, messages, err
	}
	if !forceNew {
		rooms, err := roomStore.ListRooms()
		if err != nil {
			return chat.Room{}, nil, err
		}
		for _, candidate := range rooms {
			if candidate.Workspace == workspace {
				messages, err := roomStore.LoadMessages(candidate.ID)
				return candidate, messages, err
			}
		}
	}
	roomState, err := roomStore.Create(workspace, maxWaves)
	return roomState, nil, err
}

func verifyRuntime(binary string, args ...string) error {
	path, err := exec.LookPath(binary)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("authentication check timed out")
		}
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
