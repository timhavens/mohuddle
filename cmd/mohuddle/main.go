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
		if err := orchestrator.Configure(preferences, launch); err != nil {
			return err
		}
		apiServers, err := startAPIServers(opts, roomStore, orchestrator, roomState.ID)
		if err != nil {
			_ = orchestrator.Close()
			return err
		}
		speechConfig := preferences.SpeechSettings()
		speechService := speech.New(speechConfig, speech.NewProvider(speechConfig), preferences.SetSpeechSettings)
		model := ui.New(orchestrator, roomStore, speechService)
		program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
		final, runErr := program.Run()
		speechCloseErr := speechService.Close()
		var apiCloseErr error
		for index := len(apiServers) - 1; index >= 0; index-- {
			if err := apiServers[index].Close(); err != nil && apiCloseErr == nil {
				apiCloseErr = err
			}
		}
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
	value.explicitBinaries = map[chat.Participant]bool{
		chat.Codex: seen["codex-binary"], chat.Claude: seen["claude-binary"],
		chat.Agy: seen["agy-binary"], chat.Copilot: seen["copilot-binary"],
	}
	if value.maxWaves < 1 {
		return value, fmt.Errorf("--max-waves must be at least 1")
	}
	return value, nil
}

func startAPIServers(opts options, roomStore *store.Store, orchestrator *room.Orchestrator, roomID string) ([]*api.Server, error) {
	if opts.noAPI && opts.federationListen == "" {
		return nil, nil
	}
	credentials, err := api.LoadOrCreateCredentials(api.CredentialsPath(roomStore.Root()))
	if err != nil {
		return nil, err
	}
	service, err := api.NewService(*credentials, orchestrator)
	if err != nil {
		return nil, err
	}
	audit := api.NewAuditLog(filepath.Join(roomStore.Root(), "api_audit.jsonl"))
	servers := make([]*api.Server, 0, 2)
	closeServers := func() {
		for index := len(servers) - 1; index >= 0; index-- {
			_ = servers[index].Close()
		}
	}
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
			server, err := api.StartLocal(path, service, audit)
			if err != nil {
				return nil, fmt.Errorf("start local API: %w", err)
			}
			servers = append(servers, server)
		}
	}
	if opts.federationListen != "" {
		identity, err := api.LoadOrCreateFederationIdentity(api.FederationIdentityPath(roomStore.Root()), credentials.InstanceID)
		if err != nil {
			closeServers()
			return nil, err
		}
		pairings, err := api.LoadPairingStore(api.FederationPairingsPath(roomStore.Root()), credentials.InstanceID)
		if err != nil {
			closeServers()
			return nil, err
		}
		server, err := api.StartFederation(opts.federationListen, service, audit, identity, pairings)
		if err != nil {
			closeServers()
			return nil, fmt.Errorf("start federation API: %w", err)
		}
		servers = append(servers, server)
	}
	return servers, nil
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
	if override, ok := launch[participant]; ok {
		value = mergeSettings(value, override)
	}
	return value
}

func buildAgents(opts options, roomState chat.Room, preferences *appsettings.Store, launch map[chat.Participant]chat.AgentSettings) ([]agent.Agent, error) {
	binaries := map[chat.Participant]string{
		chat.Codex: opts.codexBinary, chat.Claude: opts.claudeBinary,
		chat.Agy: opts.agyBinary, chat.Copilot: opts.copilotBinary,
	}
	result := make([]agent.Agent, 0, len(binaries))
	for _, participant := range chat.Agents() {
		binary := binaries[participant]
		if _, err := exec.LookPath(binary); err != nil {
			if opts.explicitBinaries[participant] {
				return nil, fmt.Errorf("configured %s binary %q is unavailable: %w", participant, binary, err)
			}
			continue
		}
		if roomState.Present(participant) {
			switch participant {
			case chat.Codex:
				if err := verifyRuntime(binary, "login", "status"); err != nil {
					if opts.explicitBinaries[participant] {
						return nil, fmt.Errorf("configured Codex runtime is unavailable or not authenticated: %w", err)
					}
					continue
				}
			case chat.Claude:
				if err := verifyRuntime(binary, "auth", "status"); err != nil {
					if opts.explicitBinaries[participant] {
						return nil, fmt.Errorf("configured Claude runtime is unavailable or not authenticated: %w", err)
					}
					continue
				}
			}
		}
		settings := effectiveSettings(preferences, roomState, launch, participant)
		sessionID := roomState.Sessions[participant].ID
		switch participant {
		case chat.Codex:
			result = append(result, codex.New(codex.Config{Binary: binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID}))
		case chat.Claude:
			result = append(result, claude.New(claude.Config{Binary: binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID}))
		case chat.Agy:
			result = append(result, agy.New(agy.Config{Binary: binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID}))
		case chat.Copilot:
			result = append(result, copilot.New(copilot.Config{Binary: binary, Model: settings.Model, Effort: settings.Effort, Permissions: settings.Permissions, SessionID: sessionID}))
		}
	}
	return result, nil
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
