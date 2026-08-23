package speech

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	kokoroSampleRate     = 24000
	maxKokoroAudioBytes  = 128 << 20
	kokoroStartupTimeout = 30 * time.Second
	kokoroPrebuffer      = 4 * time.Second
	// mpv retains roughly half a second at the open end of a raw PCM stream on
	// WSLg. Appending silence lets that final speech reach the device while IPC
	// supplies an observed, cancelable completion boundary.
	kokoroDrainSilence  = time.Second
	kokoroPlaybackTail  = 400 * time.Millisecond
	kokoroProducedQueue = 2
)

//go:embed kokoro_worker.py
var kokoroWorkerScript string

type KokoroProvider struct {
	config Config

	mu     sync.Mutex
	paths  kokoroPaths
	worker *kokoroWorker
	player *rawAudioPlayer
	closed bool
}

type kokoroPaths struct {
	python string
	model  string
	voices string
	mpv    string
}

type kokoroWorker struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *safeBuffer
	done    chan struct{}
	waitErr error
	voices  []string
	mu      sync.Mutex
}

type workerHeader struct {
	Type       string   `json:"type"`
	Voices     []string `json:"voices,omitempty"`
	Bytes      int      `json:"bytes,omitempty"`
	SampleRate int      `json:"sample_rate,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type synthesizedSegment struct {
	audio      []byte
	sampleRate int
	err        error
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func NewKokoroProvider(config Config) *KokoroProvider {
	return &KokoroProvider{config: config.WithDefaults()}
}

func NewProvider(config Config) Provider {
	config = config.WithDefaults()
	if config.Provider == ProviderKokoro {
		return NewKokoroProvider(config)
	}
	return NewEdgeProvider(config.PlaybackBinary)
}

func (p *KokoroProvider) Validate() error {
	paths, err := resolveKokoroPaths(p.config)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("Kokoro provider is closed")
	}
	p.paths = paths
	return nil
}

func (p *KokoroProvider) Play(ctx context.Context, voice string, segments []string) error {
	if err := validateVoice(voice); err != nil {
		return err
	}
	segments = nonEmptySegments(segments)
	if len(segments) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("Kokoro provider is closed")
	}
	if err := p.ensureWorkerLocked(); err != nil {
		return err
	}
	if !containsString(p.worker.voices, voice) {
		return fmt.Errorf("Kokoro voice %q is not installed", voice)
	}
	if err := p.ensurePlayerLocked(); err != nil {
		return err
	}
	player := p.player
	player.BeginSpeech()
	defer player.EndSpeech()
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			player.Cancel()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	producerContext, stopProducer := context.WithCancel(ctx)
	defer stopProducer()
	produced := make(chan synthesizedSegment, kokoroProducedQueue)
	go func() {
		defer close(produced)
		for _, segment := range segments {
			if producerContext.Err() != nil {
				return
			}
			audio, sampleRate, err := p.synthesizeLocked(voice, segment)
			produced <- synthesizedSegment{audio: audio, sampleRate: sampleRate, err: err}
			if err != nil {
				return
			}
		}
	}()

	// Build a small, cancelable reserve before starting the device. Kokoro runs
	// substantially faster than playback, so this absorbs a slow following
	// sentence without asking mpv to retain seconds of audio that stop/skip
	// could not flush.
	var ready []synthesizedSegment
	bufferedBytes := 0
	producerOpen := true
	for producerOpen && audioDuration(bufferedBytes) < kokoroPrebuffer {
		select {
		case item, ok := <-produced:
			producerOpen = ok
			if !ok {
				break
			}
			if item.err != nil {
				p.resetWorkerLocked()
				return item.err
			}
			if item.sampleRate != kokoroSampleRate {
				stopProducer()
				drainSynthesized(produced)
				return fmt.Errorf("Kokoro returned sample rate %d, expected %d", item.sampleRate, kokoroSampleRate)
			}
			ready = append(ready, item)
			bufferedBytes += len(item.audio)
		case <-ctx.Done():
			stopProducer()
			drainSynthesized(produced)
			p.resetPlayerLocked()
			return ctx.Err()
		}
	}

	player.StartSpeechAudio()
	started := time.Now()
	totalBytes := 0
	writeItem := func(item synthesizedSegment) error {
		if item.err != nil {
			p.resetWorkerLocked()
			return item.err
		}
		if item.sampleRate != kokoroSampleRate {
			stopProducer()
			drainSynthesized(produced)
			return fmt.Errorf("Kokoro returned sample rate %d, expected %d", item.sampleRate, kokoroSampleRate)
		}
		if err := player.Write(item.audio); err != nil {
			stopProducer()
			drainSynthesized(produced)
			p.resetPlayerLocked()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		totalBytes += len(item.audio)
		return nil
	}
	for _, item := range ready {
		if err := writeItem(item); err != nil {
			return err
		}
	}
	if producerOpen {
		for item := range produced {
			if ctx.Err() != nil {
				stopProducer()
				drainSynthesized(produced)
				p.resetPlayerLocked()
				return ctx.Err()
			}
			if err := writeItem(item); err != nil {
				return err
			}
		}
	}
	if player.SupportsObservedCompletion() {
		speechEndBytes, err := appendKokoroDrain(player)
		if err != nil {
			p.resetPlayerLocked()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		maxWait := audioDuration(totalBytes) + kokoroDrainSilence + 5*time.Second
		if err := player.WaitRendered(ctx, speechEndBytes, maxWait); err != nil {
			completionErr := resolveKokoroCompletionFailure(ctx, player, started, totalBytes, err)
			if completionErr != nil {
				p.resetPlayerLocked()
			}
			return completionErr
		}
		return nil
	}

	return waitForEstimatedPlayback(ctx, started, totalBytes, kokoroPlaybackTail)
}

func resolveKokoroCompletionFailure(
	ctx context.Context,
	player *rawAudioPlayer,
	started time.Time,
	totalBytes int,
	cause error,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !player.Running() {
		return fmt.Errorf("%w: mpv stopped before audio completion: %v", ErrPlaybackUnavailable, cause)
	}
	// IPC confirmation is an optimization for accurate queue ownership, not an
	// audio-device health check. If the player is still alive, retain FIFO order
	// with a conservative cancelable estimate and keep speech available.
	return waitForEstimatedPlayback(ctx, started, totalBytes, kokoroDrainSilence)
}

func waitForEstimatedPlayback(ctx context.Context, started time.Time, totalBytes int, tail time.Duration) error {
	remaining := audioDuration(totalBytes) - time.Since(started) + tail
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func appendKokoroDrain(player *rawAudioPlayer) (int64, error) {
	// Capture the real speech boundary before adding the drain. mpv retains
	// roughly half a second at the open end of a raw stream, so the end of the
	// drain itself is intentionally unreachable until more PCM or EOF arrives.
	speechEndBytes := player.StreamBytes()
	err := player.Write(make([]byte, audioBytes(kokoroDrainSilence)))
	return speechEndBytes, err
}

func audioDuration(bytes int) time.Duration {
	seconds := float64(bytes) / float64(kokoroSampleRate*4)
	return time.Duration(seconds * float64(time.Second))
}

func audioBytes(duration time.Duration) int {
	samples := int64(kokoroSampleRate) * int64(duration) / int64(time.Second)
	return int(samples * 4)
}

func drainSynthesized(values <-chan synthesizedSegment) {
	for range values {
	}
}

func (p *KokoroProvider) ListVoices(ctx context.Context, filter string) ([]Voice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("Kokoro provider is closed")
	}
	if err := p.ensureWorkerLocked(); err != nil {
		return nil, err
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	voices := make([]Voice, 0, len(p.worker.voices))
	for _, name := range p.worker.voices {
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		voices = append(voices, Voice{Name: name, Description: "Kokoro local voice"})
	}
	return voices, nil
}

func (p *KokoroProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var playerErr error
	if p.player != nil {
		playerErr = p.player.Finish()
		p.player = nil
	}
	p.resetWorkerLocked()
	return playerErr
}

func (p *KokoroProvider) ensureWorkerLocked() error {
	if p.worker != nil {
		select {
		case <-p.worker.done:
			p.worker = nil
		default:
			return nil
		}
	}
	if p.paths.python == "" {
		paths, err := resolveKokoroPaths(p.config)
		if err != nil {
			return err
		}
		p.paths = paths
	}
	worker, err := startKokoroWorker(p.paths, workerNice(p.config))
	if err != nil {
		return err
	}
	p.worker = worker
	return nil
}

func (p *KokoroProvider) ensurePlayerLocked() error {
	if p.player != nil && p.player.Running() {
		return nil
	}
	player, err := startKokoroPlayer(p.paths.mpv)
	if err != nil {
		return err
	}
	p.player = player
	return nil
}

func (p *KokoroProvider) synthesizeLocked(voice, text string) ([]byte, int, error) {
	request := map[string]any{"op": "synthesize", "voice": voice, "text": text, "speed": 1.0, "language": "en-us"}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, 0, err
	}
	payload = append(payload, '\n')
	if _, err := p.worker.stdin.Write(payload); err != nil {
		return nil, 0, fmt.Errorf("send Kokoro request: %w", err)
	}
	header, err := readWorkerHeader(p.worker.stdout)
	if err != nil {
		return nil, 0, fmt.Errorf("read Kokoro response: %w: %s", err, strings.TrimSpace(p.worker.stderr.String()))
	}
	if header.Type == "error" {
		return nil, 0, fmt.Errorf("Kokoro synthesis: %s", header.Error)
	}
	if header.Type != "audio" || header.Bytes < 1 || header.Bytes > maxKokoroAudioBytes {
		return nil, 0, fmt.Errorf("invalid Kokoro audio response: type=%q bytes=%d", header.Type, header.Bytes)
	}
	audio := make([]byte, header.Bytes)
	if _, err := io.ReadFull(p.worker.stdout, audio); err != nil {
		return nil, 0, fmt.Errorf("read Kokoro audio: %w", err)
	}
	return audio, header.SampleRate, nil
}

func startKokoroWorker(paths kokoroPaths, nice int) (*kokoroWorker, error) {
	command := exec.Command(paths.python, "-u", "-c", kokoroWorkerScript, paths.model, paths.voices)
	prepareProcess(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &safeBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start Kokoro worker: %w", err)
	}
	// Priority is an optimization, not a prerequisite. Platforms that cannot
	// adjust it still get a functional local provider.
	_ = setProcessNice(command.Process, nice)
	worker := &kokoroWorker{command: command, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 64*1024), stderr: stderr, done: make(chan struct{})}
	go func() {
		worker.mu.Lock()
		worker.waitErr = command.Wait()
		worker.mu.Unlock()
		close(worker.done)
	}()

	ready := make(chan struct {
		header workerHeader
		err    error
	}, 1)
	go func() {
		header, err := readWorkerHeader(worker.stdout)
		ready <- struct {
			header workerHeader
			err    error
		}{header: header, err: err}
	}()
	select {
	case result := <-ready:
		if result.err != nil || result.header.Type != "ready" || len(result.header.Voices) == 0 {
			worker.stop()
			if result.err != nil {
				return nil, fmt.Errorf("start Kokoro worker: %w: %s", result.err, strings.TrimSpace(stderr.String()))
			}
			return nil, fmt.Errorf("start Kokoro worker: invalid ready response")
		}
		worker.voices = append([]string(nil), result.header.Voices...)
		sort.Strings(worker.voices)
		return worker, nil
	case <-time.After(kokoroStartupTimeout):
		worker.stop()
		return nil, fmt.Errorf("start Kokoro worker: timed out after %s", kokoroStartupTimeout)
	}
}

func (w *kokoroWorker) stop() {
	_ = w.stdin.Close()
	select {
	case <-w.done:
		return
	case <-time.After(750 * time.Millisecond):
		interruptProcess(w.command.Process)
	}
	select {
	case <-w.done:
	case <-time.After(750 * time.Millisecond):
		killProcess(w.command.Process)
		<-w.done
	}
}

func (p *KokoroProvider) resetWorkerLocked() {
	if p.worker != nil {
		p.worker.stop()
		p.worker = nil
	}
}

func (p *KokoroProvider) resetPlayerLocked() {
	if p.player != nil {
		p.player.Cancel()
		p.player = nil
	}
}

func readWorkerHeader(reader *bufio.Reader) (workerHeader, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return workerHeader{}, err
	}
	var header workerHeader
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil {
		return workerHeader{}, err
	}
	return header, nil
}

func resolveKokoroPaths(config Config) (kokoroPaths, error) {
	root, err := defaultKokoroRoot()
	if err != nil {
		return kokoroPaths{}, err
	}
	python := config.PythonBinary
	if python == "" {
		python = filepath.Join(root, "venv", "bin", "python")
	}
	python, err = resolveExecutable(python)
	if err != nil {
		return kokoroPaths{}, fmt.Errorf("Kokoro Python is unavailable: %w", err)
	}
	model := config.ModelPath
	if model == "" {
		model = filepath.Join(root, "kokoro-v1.0.onnx")
	}
	model, err = resolveRegularFile(model)
	if err != nil {
		return kokoroPaths{}, fmt.Errorf("Kokoro model is unavailable: %w", err)
	}
	voices := config.VoicesPath
	if voices == "" {
		voices = filepath.Join(root, "voices-v1.0.bin")
	}
	voices, err = resolveRegularFile(voices)
	if err != nil {
		return kokoroPaths{}, fmt.Errorf("Kokoro voices are unavailable: %w", err)
	}
	mpv := config.PlayerBinary
	if mpv == "" {
		mpv = "mpv"
	}
	mpv, err = resolveExecutable(mpv)
	if err != nil {
		return kokoroPaths{}, fmt.Errorf("mpv is unavailable: %w", err)
	}
	return kokoroPaths{python: python, model: model, voices: voices, mpv: mpv}, nil
}

func defaultKokoroRoot() (string, error) {
	dataRoot := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataRoot = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataRoot, "mohuddle", "tts", "kokoro"), nil
}

func resolveRegularFile(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", absolute)
	}
	return absolute, nil
}

func workerNice(config Config) int {
	config = config.WithDefaults()
	if config.WorkerNice == nil {
		return DefaultWorkerNice
	}
	return *config.WorkerNice
}

func containsString(values []string, value string) bool {
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

func nonEmptySegments(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

var _ Provider = (*KokoroProvider)(nil)
