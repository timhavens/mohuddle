package speech

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKokoroPlayerUsesOnlyPulseOnWSL(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	arguments := kokoroPlayerArguments("/tmp/mohuddle-mpv-test/socket")
	if !slices.Contains(arguments, "--ao=pulse") {
		t.Fatalf("arguments=%v", arguments)
	}
	if !slices.Contains(arguments, "--input-ipc-server=/tmp/mohuddle-mpv-test/socket") {
		t.Fatalf("arguments=%v", arguments)
	}
}

func TestPlayerFailureDetailCondensesWSLgError(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	stderr := `ALSA lib pulse.c: PulseAudio: Unable to connect: Timeout
Cannot connect to server socket
JackShmReadWritePtr noise
couldn't open play stream: No such file or directory`
	detail := playerFailureDetail(stderr)
	if !strings.Contains(detail, "WSLg PulseAudio is unavailable") || !strings.Contains(detail, "/speak all") || strings.Contains(detail, "ALSA lib") {
		t.Fatalf("detail=%q", detail)
	}
}

func TestPlayerFailureDetailRecognizesEmptyWSLgRace(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	if detail := playerFailureDetail(""); !strings.Contains(detail, "WSLg PulseAudio is unavailable") || !strings.Contains(detail, "/speak all") {
		t.Fatalf("detail=%q", detail)
	}
}

func TestKokoroPlayerFeedsSilenceOnlyWhileIdleOnWSL(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Debian")
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "audio.raw")
	playerPath := filepath.Join(dir, "mpv")
	writeExecutable(t, playerPath, "#!/bin/sh\ncat > \"$MOHUDDLE_PLAYER_AUDIO\"\n")
	t.Setenv("MOHUDDLE_PLAYER_AUDIO", audioPath)

	player, err := startKokoroPlayer(playerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer player.Cancel()
	waitForFileSize(t, audioPath, 1)

	player.BeginSpeech()
	player.StartSpeechAudio()
	time.Sleep(2 * kokoroIdleSilenceInterval)
	before, err := os.Stat(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * kokoroIdleSilenceInterval)
	after, err := os.Stat(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("idle silence continued during speech: before=%d after=%d", before.Size(), after.Size())
	}

	player.EndSpeech()
	waitForFileSize(t, audioPath, after.Size()+1)
}

func TestKokoroPlayerExpiresAfterBoundedIdleWindow(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	dir := t.TempDir()
	playerPath := filepath.Join(dir, "mpv")
	writeExecutable(t, playerPath, "#!/bin/sh\ncat >/dev/null\n")

	player, err := startKokoroPlayerWithIdleTimeout(playerPath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-player.done:
	case <-time.After(time.Second):
		player.Cancel()
		t.Fatal("idle player did not expire")
	}
	if player.Running() {
		t.Fatal("expired player still reports running")
	}
}

func TestKokoroPlayerDoesNotExpireDuringSpeech(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	dir := t.TempDir()
	playerPath := filepath.Join(dir, "mpv")
	writeExecutable(t, playerPath, "#!/bin/sh\ncat >/dev/null\n")

	player, err := startKokoroPlayerWithIdleTimeout(playerPath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer player.Cancel()
	player.BeginSpeech()
	player.StartSpeechAudio()
	time.Sleep(250 * time.Millisecond)
	if !player.Running() {
		t.Fatal("player expired during active speech")
	}
	player.EndSpeech()
	select {
	case <-player.done:
	case <-time.After(time.Second):
		t.Fatal("player did not expire after speech ended")
	}
}

func TestKokoroPlayerWaitsForObservedAudioPosition(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "mpv.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		positions := []float64{0.25, 0.75, 1.0}
		for _, position := range positions {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				serverDone <- err
				return
			}
			var request struct {
				RequestID int64 `json:"request_id"`
			}
			if err := json.Unmarshal(line, &request); err != nil {
				serverDone <- err
				return
			}
			response := map[string]any{
				"data": position, "error": "success", "request_id": request.RequestID,
			}
			if err := json.NewEncoder(connection).Encode(response); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	player := &rawAudioPlayer{ipcPath: socketPath, done: make(chan struct{})}
	if err := player.WaitRendered(context.Background(), int64(audioBytes(time.Second)), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestKokoroIdleSilenceRejectsShortWrite(t *testing.T) {
	writer := &shortWriteCloser{}
	player := &rawAudioPlayer{stdin: writer, stderr: &safeBuffer{}}
	err := player.writeIdleSilence(make([]byte, 8))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error=%v", err)
	}
	if got := player.StreamBytes(); got != 7 {
		t.Fatalf("stream bytes=%d, want 7", got)
	}
}

type shortWriteCloser struct{}

func (w *shortWriteCloser) Write(value []byte) (int, error) {
	return len(value) - 1, nil
}

func (w *shortWriteCloser) Close() error {
	return nil
}

func waitForFileSize(t *testing.T, path string, minimum int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Size() >= minimum {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	t.Fatalf("%s size=%d, want at least %d", path, info.Size(), minimum)
}
