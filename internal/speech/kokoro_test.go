package speech

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKokoroDrainCompletionTargetExcludesOpenStreamTail(t *testing.T) {
	output := &bufferWriteCloser{}
	player := &rawAudioPlayer{stdin: output, stderr: &safeBuffer{}}
	const speechBytes = int64(kokoroSampleRate * 4)
	player.streamBytes.Store(speechBytes)

	target, err := appendKokoroDrain(player)
	if err != nil {
		t.Fatal(err)
	}
	if target != speechBytes {
		t.Fatalf("completion target=%d, want speech boundary %d", target, speechBytes)
	}
	wantTotal := speechBytes + int64(audioBytes(kokoroDrainSilence))
	if got := player.StreamBytes(); got != wantTotal {
		t.Fatalf("stream bytes=%d, want speech plus drain %d", got, wantTotal)
	}
}

func TestKokoroDrainExceedsObservedWSLRetainedTail(t *testing.T) {
	const observedMaximum = 635 * time.Millisecond
	const safetyMargin = 100 * time.Millisecond
	if kokoroDrainSilence < observedMaximum+safetyMargin {
		t.Fatalf("drain silence=%s, want at least %s", kokoroDrainSilence, observedMaximum+safetyMargin)
	}
}

func TestKokoroCompletionTimeoutKeepsRunningPlayerAvailable(t *testing.T) {
	player := &rawAudioPlayer{done: make(chan struct{})}
	err := resolveKokoroCompletionFailure(
		context.Background(), player, time.Now().Add(-2*time.Second),
		audioBytes(time.Second), context.DeadlineExceeded,
	)
	if err != nil {
		t.Fatalf("running player completion fallback returned %v", err)
	}
}

func TestKokoroCompletionFailureMarksStoppedPlayerUnavailable(t *testing.T) {
	done := make(chan struct{})
	close(done)
	player := &rawAudioPlayer{done: done}
	err := resolveKokoroCompletionFailure(
		context.Background(), player, time.Now(),
		audioBytes(time.Second), errors.New("IPC closed"),
	)
	if !errors.Is(err, ErrPlaybackUnavailable) {
		t.Fatalf("error=%v, want playback unavailable", err)
	}
}

type bufferWriteCloser struct {
	bytes.Buffer
}

func (b *bufferWriteCloser) Close() error {
	return nil
}

func TestKokoroProviderUsesOnePlayerForAllSegments(t *testing.T) {
	skipShellProcessTest(t)
	t.Setenv("WSL_DISTRO_NAME", "")
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "audio.raw")
	startsPath := filepath.Join(dir, "starts.txt")
	python := filepath.Join(dir, "python")
	mpv := filepath.Join(dir, "mpv")
	model := filepath.Join(dir, "model.onnx")
	voices := filepath.Join(dir, "voices.bin")
	writeExecutable(t, python, `#!/bin/sh
printf '%s\n' '{"type":"ready","voices":["voice-one","voice-two"]}'
while IFS= read -r request; do
  printf '%s\n' '{"type":"audio","bytes":4,"sample_rate":24000}'
  printf 'abcd'
done
`)
	writeExecutable(t, mpv, `#!/bin/sh
printf 'start\n' >> "$MOHUDDLE_PLAYER_STARTS"
cat > "$MOHUDDLE_PLAYER_AUDIO"
`)
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voices, []byte("voices"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_PLAYER_AUDIO", audioPath)
	t.Setenv("MOHUDDLE_PLAYER_STARTS", startsPath)

	zero := 0
	provider := NewKokoroProvider(Config{
		Provider: ProviderKokoro, PythonBinary: python, ModelPath: model,
		VoicesPath: voices, PlayerBinary: mpv, WorkerNice: &zero,
	})
	defer provider.Close()
	if err := provider.Validate(); err != nil {
		t.Fatal(err)
	}
	listed, err := provider.ListVoices(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "voice-two" {
		t.Fatalf("voices=%+v", listed)
	}
	if err := provider.Play(context.Background(), "voice-one", []string{"First.", "Second."}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Play(context.Background(), "voice-two", []string{"Third."}); err != nil {
		t.Fatal(err)
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "abcdabcdabcd" {
		t.Fatalf("audio=%q", audio)
	}
	starts, err := os.ReadFile(startsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(starts), "start") != 1 {
		t.Fatalf("player starts=%q", starts)
	}
}

func TestKokoroProviderSoftCancellationDiscardsCurrentSegment(t *testing.T) {
	skipShellProcessTest(t)
	t.Setenv("WSL_DISTRO_NAME", "")
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "audio.raw")
	python := filepath.Join(dir, "python")
	mpv := filepath.Join(dir, "mpv")
	model := filepath.Join(dir, "model.onnx")
	voices := filepath.Join(dir, "voices.bin")
	writeExecutable(t, python, `#!/bin/sh
printf '%s\n' '{"type":"ready","voices":["voice-one"]}'
while IFS= read -r request; do
  sleep 0.2
  printf '%s\n' '{"type":"audio","bytes":4,"sample_rate":24000}'
  printf 'abcd'
done
`)
	writeExecutable(t, mpv, `#!/bin/sh
cat > "$MOHUDDLE_PLAYER_AUDIO"
`)
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voices, []byte("voices"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_PLAYER_AUDIO", audioPath)
	zero := 0
	provider := NewKokoroProvider(Config{
		Provider: ProviderKokoro, PythonBinary: python, ModelPath: model,
		VoicesPath: voices, PlayerBinary: mpv, WorkerNice: &zero,
	})
	defer provider.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Play(ctx, "voice-one", []string{"First.", "Never synthesize this."}) }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("soft cancellation did not return after the active segment")
	}
	if audio, err := os.ReadFile(audioPath); err == nil && len(audio) != 0 {
		t.Fatalf("cancelled audio was written: %q", audio)
	}
}

func TestKokoroProviderCancellationFlushesAndRecreatesPersistentPlayer(t *testing.T) {
	skipShellProcessTest(t)
	t.Setenv("WSL_DISTRO_NAME", "")
	dir := t.TempDir()
	startsPath := filepath.Join(dir, "starts.txt")
	python := filepath.Join(dir, "python")
	mpv := filepath.Join(dir, "mpv")
	model := filepath.Join(dir, "model.onnx")
	voices := filepath.Join(dir, "voices.bin")
	writeExecutable(t, python, `#!/bin/sh
printf '%s\n' '{"type":"ready","voices":["voice-one"]}'
while IFS= read -r request; do
  printf '%s\n' '{"type":"audio","bytes":9600,"sample_rate":24000}'
  dd if=/dev/zero bs=9600 count=1 2>/dev/null
done
`)
	writeExecutable(t, mpv, `#!/bin/sh
printf 'start\n' >> "$MOHUDDLE_PLAYER_STARTS"
cat >/dev/null
`)
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voices, []byte("voices"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOHUDDLE_PLAYER_STARTS", startsPath)
	zero := 0
	provider := NewKokoroProvider(Config{
		Provider: ProviderKokoro, PythonBinary: python, ModelPath: model,
		VoicesPath: voices, PlayerBinary: mpv, WorkerNice: &zero,
	})
	defer provider.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Play(ctx, "voice-one", []string{"Cancel this."}) }()
	// Cancel only after the first persistent player has actually started. A
	// fixed sleep can expire before process startup under the race detector,
	// making the test observe one legitimate start instead of the two it is
	// intended to verify.
	waitForFileSize(t, startsPath, 1)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("playback cancellation exceeded one second")
	}
	if err := provider.Play(context.Background(), "voice-one", []string{"Play after cancellation."}); err != nil {
		t.Fatal(err)
	}
	starts, err := os.ReadFile(startsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(starts), "start") != 2 {
		t.Fatalf("player starts=%q", starts)
	}
}
