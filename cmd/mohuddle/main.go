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
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/ui"
)

type options struct {
	workspace    string
	roomID       string
	newRoom      bool
	showVersion  bool
	maxTurns     int
	codexBinary  string
	claudeBinary string
	codexModel   string
	claudeModel  string
	stateDir     string
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

	nextRoomID := opts.roomID
	forceNew := opts.newRoom
	for {
		roomState, messages, err := selectRoom(roomStore, workspace, nextRoomID, forceNew, opts.maxTurns)
		if err != nil {
			return err
		}
		codexAgent := codex.New(codex.Config{Binary: opts.codexBinary, Model: opts.codexModel, SessionID: roomState.Sessions[chat.Codex].ID})
		claudeAgent := claude.New(claude.Config{Binary: opts.claudeBinary, Model: opts.claudeModel, SessionID: roomState.Sessions[chat.Claude].ID})
		orchestrator, err := room.New(roomState, messages, roomStore, codexAgent, claudeAgent)
		if err != nil {
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
	flag.StringVar(&value.stateDir, "state-dir", "", "room state directory")
	flag.Parse()
	if value.maxTurns < 1 {
		value.maxTurns = 4
	}
	return value
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
