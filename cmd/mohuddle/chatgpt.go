package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chatgpt"
	"github.com/timhavens/mohuddle/internal/store"
)

func runChatGPTCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "serve" && args[0] != "doctor") {
		return fmt.Errorf("usage: mohuddle chatgpt serve|doctor [--room ROOM] [--state-dir DIR] [--connection FILE]")
	}
	flags := flag.NewFlagSet("mohuddle chatgpt "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("connection", "", "private connection file created by /join @chatgpt in MoHuddle")
	roomID := flags.String("room", "", "select an authorized room when more than one is open")
	stateDir := flags.String("state-dir", "", "MoHuddle state directory (default: personal state directory)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (*path != "" && (*roomID != "" || *stateDir != "")) {
		return fmt.Errorf("use --connection by itself, or select a room with --room and --state-dir")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	bridge, err := findChatGPTRoom(ctx, *path, *stateDir, *roomID)
	if err != nil {
		return err
	}
	if args[0] == "doctor" {
		if err := bridge.Doctor(ctx); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, "Private ChatGPT connection verified. Grant is room-scoped and room controls are denied. No messages were read or sent.")
		return err
	}
	// Standard output is exclusively MCP framing. No public listening mode exists.
	return bridge.Server().Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: os.Stdout, MaxLineLength: api.MaxFrameBytes})
}

func findChatGPTRoom(ctx context.Context, path, stateDir, roomID string) (*chatgpt.Bridge, error) {
	if path != "" {
		return chatgpt.NewFromFile(path)
	}
	if roomID != "" && (roomID == "." || roomID == ".." || strings.ContainsAny(roomID, `/\`)) {
		return nil, fmt.Errorf("--room must be a room ID, not a path")
	}
	if stateDir == "" {
		var err error
		stateDir, err = store.DefaultStateDir()
		if err != nil {
			return nil, err
		}
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cannot inspect the MoHuddle state directory: %w", err)
	}
	var found *chatgpt.Bridge
	var rooms []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "chatgpt-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "chatgpt-"), ".json")
		if roomID != "" && id != roomID {
			continue
		}
		bridge, err := chatgpt.NewFromFile(filepath.Join(stateDir, name))
		if err != nil || bridge.RoomID() != id {
			continue
		}
		// Validate the live grant without claiming a seat or reading room data.
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = bridge.Doctor(probe)
		cancel()
		if err != nil {
			continue
		}
		found = bridge
		rooms = append(rooms, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, fmt.Errorf("no active authorized ChatGPT room found; open MoHuddle, run /join @chatgpt, then retry")
	}
	if len(rooms) > 1 {
		return nil, fmt.Errorf("multiple authorized rooms are open (%s); select one with --room ROOM", strings.Join(rooms, ", "))
	}
	return found, nil
}
