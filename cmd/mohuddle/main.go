package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/agent/claude"
	"github.com/timhavens/mohuddle/internal/agent/codex"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/room"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/ui"
)

type options struct {
	workspace         string
	roomID            string
	newRoom           bool
	showVersion       bool
	maxTurns          int
	codexBinary       string
	claudeBinary      string
	codexModel        string
	claudeModel       string
	codexEffort       string
	claudeEffort      string
	codexPermissions  string
	claudePermissions string
	stateDir          string
	configPath        string
}

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mohuddle:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := parseFlags()
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
	if err := verifyRuntime(opts.codexBinary, "login", "status"); err != nil {
		return fmt.Errorf("Codex is unavailable or not authenticated: %w", err)
	}
	if err := verifyRuntime(opts.claudeBinary, "auth", "status"); err != nil {
		return fmt.Errorf("Claude is unavailable or not authenticated: %w", err)
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
		roomState, messages, err := selectRoom(roomStore, workspace, nextRoomID, forceNew, opts.maxTurns)
		if err != nil {
			return err
		}
		codexSettings := preferences.Effective(roomState, chat.Codex)
		claudeSettings := preferences.Effective(roomState, chat.Claude)
		if value, ok := launch[chat.Codex]; ok {
			codexSettings = mergeSettings(codexSettings, value)
		}
		if value, ok := launch[chat.Claude]; ok {
			claudeSettings = mergeSettings(claudeSettings, value)
		}
		if !preferences.FullAccessAcknowledged() && (codexSettings.Permissions == chat.PermissionFull || claudeSettings.Permissions == chat.PermissionFull) {
			return fmt.Errorf("saved full access requires a one-time acknowledgement in /settings")
		}
		codexAgent := codex.New(codex.Config{Binary: opts.codexBinary, Model: codexSettings.Model, Effort: codexSettings.Effort, Permissions: codexSettings.Permissions, SessionID: roomState.Sessions[chat.Codex].ID})
		claudeAgent := claude.New(claude.Config{Binary: opts.claudeBinary, Model: claudeSettings.Model, Effort: claudeSettings.Effort, Permissions: claudeSettings.Permissions, SessionID: roomState.Sessions[chat.Claude].ID})
		orchestrator, err := room.New(roomState, messages, roomStore, codexAgent, claudeAgent)
		if err != nil {
			return err
		}
		if err := orchestrator.Configure(preferences, launch); err != nil {
			return err
		}
		model := ui.New(orchestrator, roomStore)
		program := tea.NewProgram(model, tea.WithAltScreen())
		final, runErr := program.Run()
		closeErr := orchestrator.Close()
		if runErr != nil {
			return runErr
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
	var value options
	flag.StringVar(&value.workspace, "workspace", ".", "initial workspace directory")
	flag.StringVar(&value.roomID, "room", "", "resume a saved room by ID")
	flag.BoolVar(&value.newRoom, "new", false, "start a new room instead of resuming the latest room for this workspace")
	flag.BoolVar(&value.showVersion, "version", false, "print the MoHuddle version and exit")
	flag.IntVar(&value.maxTurns, "max-turns", 4, "maximum AI turns in an untargeted round")
	flag.StringVar(&value.codexBinary, "codex-binary", "codex", "Codex CLI binary")
	flag.StringVar(&value.claudeBinary, "claude-binary", "claude", "Claude Code CLI binary")
	flag.StringVar(&value.codexModel, "codex-model", "", "Codex model override (default: CLI configuration)")
	flag.StringVar(&value.claudeModel, "claude-model", "", "Claude model override (default: CLI configuration)")
	flag.StringVar(&value.codexEffort, "codex-effort", "", "Codex effort override")
	flag.StringVar(&value.claudeEffort, "claude-effort", "", "Claude effort override")
	flag.StringVar(&value.codexPermissions, "codex-permissions", "", "Codex permissions: read-only, workspace, or full")
	flag.StringVar(&value.claudePermissions, "claude-permissions", "", "Claude permissions: read-only, workspace, or full")
	flag.StringVar(&value.stateDir, "state-dir", "", "room state directory")
	flag.StringVar(&value.configPath, "config", "", "personal settings file")
	flag.Parse()
	if value.maxTurns < 1 {
		value.maxTurns = 4
	}
	return value
}

func launchSettings(opts options) (map[chat.Participant]chat.AgentSettings, error) {
	result := make(map[chat.Participant]chat.AgentSettings, 2)
	values := []struct {
		participant                chat.Participant
		model, effort, permissions string
	}{
		{chat.Codex, opts.codexModel, opts.codexEffort, opts.codexPermissions},
		{chat.Claude, opts.claudeModel, opts.claudeEffort, opts.claudePermissions},
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

func selectRoom(roomStore *store.Store, workspace, roomID string, forceNew bool, maxTurns int) (chat.Room, []chat.Message, error) {
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
	roomState, err := roomStore.Create(workspace, maxTurns)
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
