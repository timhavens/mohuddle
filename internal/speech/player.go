package speech

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	kokoroIdleSilenceInterval = 100 * time.Millisecond
	kokoroIdleWriteTimeout    = 500 * time.Millisecond
	kokoroPlayerIdleTimeout   = 30 * time.Second
)

type rawAudioPlayer struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stderr  *safeBuffer
	done    chan struct{}

	mu       sync.Mutex
	waitErr  error
	closed   bool
	stopOnce sync.Once
	ipcPath  string
	ipcDir   string

	writeMu      sync.Mutex
	speechActive atomic.Bool
	speechInUse  atomic.Bool
	streamBytes  atomic.Int64
	renderedAt   atomic.Int64
	idleSince    atomic.Int64
	idleReset    chan struct{}
	idleTimeout  time.Duration
	feedSilence  bool
}

func startKokoroPlayer(binary string) (*rawAudioPlayer, error) {
	return startKokoroPlayerWithIdleTimeout(binary, kokoroPlayerIdleTimeout)
}

func startKokoroPlayerWithIdleTimeout(binary string, idleTimeout time.Duration) (*rawAudioPlayer, error) {
	var ipcDir string
	var ipcPath string
	if usesWSLPulse() {
		var err error
		ipcDir, err = os.MkdirTemp("", "mohuddle-mpv-ipc-")
		if err != nil {
			return nil, fmt.Errorf("create mpv IPC directory: %w", err)
		}
		ipcPath = filepath.Join(ipcDir, "socket")
	}
	cleanupIPC := func() {
		if ipcDir != "" {
			_ = os.RemoveAll(ipcDir)
		}
	}
	command := exec.Command(binary, kokoroPlayerArguments(ipcPath)...)
	prepareProcess(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		cleanupIPC()
		return nil, fmt.Errorf("open mpv stdin: %w", err)
	}
	stderr := &safeBuffer{}
	command.Stdout = io.Discard
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		cleanupIPC()
		return nil, fmt.Errorf("start mpv: %w", err)
	}
	player := &rawAudioPlayer{
		command: command, stdin: stdin, stderr: stderr, done: make(chan struct{}),
		ipcPath: ipcPath, ipcDir: ipcDir,
		idleReset: make(chan struct{}, 1), idleTimeout: idleTimeout, feedSilence: usesWSLPulse(),
	}
	player.idleSince.Store(time.Now().UnixNano())
	go func() {
		err := command.Wait()
		player.mu.Lock()
		player.waitErr = err
		player.mu.Unlock()
		close(player.done)
		cleanupIPC()
	}()
	go player.manageIdleLifetime()
	return player, nil
}

func kokoroPlayerArguments(ipcPath ...string) []string {
	arguments := []string{
		"--no-config", "--no-video", "--really-quiet", "--cache=no",
		"--demuxer=rawaudio", "--demuxer-rawaudio-format=floatle",
		fmt.Sprintf("--demuxer-rawaudio-rate=%d", kokoroSampleRate),
		"--demuxer-rawaudio-channels=mono", "-",
	}
	if usesWSLPulse() {
		// WSLg exposes PulseAudio. Avoid mpv's noisy fallback through JACK and
		// hardware ALSA devices when the WSLg server itself is unavailable.
		arguments = append([]string{"--ao=pulse"}, arguments...)
	}
	if len(ipcPath) > 0 && ipcPath[0] != "" {
		arguments = append([]string{"--input-ipc-server=" + ipcPath[0]}, arguments...)
	}
	return arguments
}

func usesWSLPulse() bool {
	return os.Getenv("WSL_DISTRO_NAME") != "" && os.Getenv("PULSE_SERVER") != ""
}

func (p *rawAudioPlayer) manageIdleLifetime() {
	idleTimer := time.NewTimer(p.idleTimeout)
	defer idleTimer.Stop()
	var silence []byte
	var silenceTicker *time.Ticker
	var silenceTick <-chan time.Time
	if p.feedSilence {
		silence = make([]byte, kokoroSampleRate*4*int(kokoroIdleSilenceInterval)/int(time.Second))
		silenceTicker = time.NewTicker(kokoroIdleSilenceInterval)
		silenceTick = silenceTicker.C
		defer silenceTicker.Stop()
	}
	for {
		select {
		case <-p.idleReset:
			resetTimer(idleTimer, p.idleTimeout)
		case <-silenceTick:
			if err := p.writeIdleSilence(silence); err != nil {
				p.Cancel()
				return
			}
		case <-idleTimer.C:
			if p.speechInUse.Load() || p.speechActive.Load() {
				idleTimer.Reset(p.idleTimeout)
				continue
			}
			if remaining := p.idleRemaining(); remaining > 0 {
				idleTimer.Reset(remaining)
				continue
			}
			if p.expireIdle() {
				return
			}
			idleTimer.Reset(p.idleTimeout)
		case <-p.done:
			return
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func (p *rawAudioPlayer) expireIdle() bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.speechInUse.Load() || p.speechActive.Load() || p.idleRemaining() > 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return true
	}
	p.closed = true
	_ = p.stdin.Close()
	return true
}

func (p *rawAudioPlayer) idleRemaining() time.Duration {
	since := p.idleSince.Load()
	if since == 0 {
		return p.idleTimeout
	}
	remaining := p.idleTimeout - time.Since(time.Unix(0, since))
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (p *rawAudioPlayer) resetIdleDeadline() {
	select {
	case p.idleReset <- struct{}{}:
	default:
	}
}

func (p *rawAudioPlayer) BeginSpeech() {
	p.speechInUse.Store(true)
	p.idleSince.Store(time.Now().UnixNano())
	p.resetIdleDeadline()
}

func (p *rawAudioPlayer) StartSpeechAudio() {
	p.speechActive.Store(true)
}

func (p *rawAudioPlayer) EndSpeech() {
	p.speechActive.Store(false)
	p.idleSince.Store(time.Now().UnixNano())
	p.speechInUse.Store(false)
	p.resetIdleDeadline()
}

func (p *rawAudioPlayer) writeIdleSilence(silence []byte) error {
	if len(silence) == 0 {
		return nil
	}
	if p.speechActive.Load() || !p.writeMu.TryLock() {
		return nil
	}
	defer p.writeMu.Unlock()
	if p.speechActive.Load() {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	stdin := p.stdin
	p.mu.Unlock()
	if deadline, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadline.SetWriteDeadline(time.Now().Add(kokoroIdleWriteTimeout))
		defer deadline.SetWriteDeadline(time.Time{}) //nolint:errcheck // best-effort reset on an internal pipe
	}
	written, err := stdin.Write(silence)
	p.streamBytes.Add(int64(written))
	if err != nil {
		return err
	}
	if written != len(silence) {
		return io.ErrShortWrite
	}
	return nil
}

func (p *rawAudioPlayer) Write(value []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("mpv stdin is closed")
	}
	stdin := p.stdin
	p.mu.Unlock()
	written, err := io.Copy(stdin, bytes.NewReader(value))
	p.streamBytes.Add(written)
	if err != nil || written != int64(len(value)) {
		return fmt.Errorf("%w: %s", ErrPlaybackUnavailable, playerFailureDetail(p.stderr.String()))
	}
	return nil
}

func (p *rawAudioPlayer) SupportsObservedCompletion() bool {
	return p.ipcPath != ""
}

func (p *rawAudioPlayer) StreamBytes() int64 {
	return p.streamBytes.Load()
}

type mpvIPCResponse struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	RequestID int64           `json:"request_id"`
}

func (p *rawAudioPlayer) WaitRendered(ctx context.Context, targetBytes int64, maxWait time.Duration) error {
	if p.ipcPath == "" {
		return errors.New("mpv completion IPC is unavailable")
	}
	if maxWait <= 0 {
		maxWait = 5 * time.Second
	}
	waitContext, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	connection, err := p.dialIPC(waitContext)
	if err != nil {
		return err
	}
	defer connection.Close()

	reader := bufio.NewReader(connection)
	targetSeconds := float64(targetBytes) / float64(kokoroSampleRate*4)
	const completionToleranceSeconds = 0.005
	var requestID int64
	for {
		requestID++
		_ = connection.SetDeadline(time.Now().Add(500 * time.Millisecond))
		request := map[string]any{
			"command":    []string{"get_property", "audio-pts/full"},
			"request_id": requestID,
		}
		if err := json.NewEncoder(connection).Encode(request); err != nil {
			return fmt.Errorf("query mpv audio position: %w", err)
		}
		response, err := readMPVResponse(reader, requestID)
		if err != nil {
			return fmt.Errorf("read mpv audio position: %w", err)
		}
		if response.Error == "success" && len(response.Data) > 0 && string(response.Data) != "null" {
			var position float64
			if err := json.Unmarshal(response.Data, &position); err != nil {
				return fmt.Errorf("decode mpv audio position: %w", err)
			}
			if position+completionToleranceSeconds >= targetSeconds {
				p.renderedAt.Store(time.Now().UnixNano())
				return nil
			}
		} else if response.Error != "success" && response.Error != "property unavailable" {
			return fmt.Errorf("query mpv audio position: %s", response.Error)
		}
		select {
		case <-waitContext.Done():
			return waitContext.Err()
		case <-p.done:
			return fmt.Errorf("mpv stopped before audio completion: %s", playerFailureDetail(p.stderr.String()))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *rawAudioPlayer) dialIPC(ctx context.Context) (net.Conn, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("unix", p.ipcPath, 100*time.Millisecond)
		if err == nil {
			return connection, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.done:
			return nil, fmt.Errorf("mpv stopped before IPC became ready: %s", playerFailureDetail(p.stderr.String()))
		case <-ticker.C:
		}
	}
}

func readMPVResponse(reader *bufio.Reader, requestID int64) (mpvIPCResponse, error) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return mpvIPCResponse{}, err
		}
		var response mpvIPCResponse
		if err := json.Unmarshal(line, &response); err != nil {
			continue
		}
		if response.RequestID == requestID {
			return response, nil
		}
	}
}

func playerFailureDetail(stderr string) string {
	detail := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(detail, "PulseAudio: Unable to connect"),
		strings.Contains(detail, "pa_context_connect() failed"),
		strings.Contains(detail, "Connection refused"):
		return "WSLg PulseAudio is unavailable; restart WSL, then run /speak all"
	case strings.Contains(detail, "couldn't open play stream"):
		return "the audio output device could not be opened"
	case detail == "" && usesWSLPulse():
		// mpv can close stdin before its audio-thread diagnostic reaches the
		// captured stderr buffer. Under WSL the selected output is Pulse only.
		return "WSLg PulseAudio is unavailable; restart WSL, then run /speak all"
	}
	lines := strings.Split(detail, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if len(line) > 240 {
			line = line[:240] + "..."
		}
		return line
	}
	return "mpv stopped before accepting audio"
}

func (p *rawAudioPlayer) Running() bool {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *rawAudioPlayer) Finish() error {
	// A short sentinel follows the final speech boundary, but mpv's reported
	// position can still lead the physical output buffer. When a caller closes
	// immediately after Play (notably tts-smoke), keep the stream alive long
	// enough for that already-written sentinel to displace all remaining speech.
	if renderedAt := p.renderedAt.Load(); renderedAt != 0 && p.Running() {
		remaining := kokoroDrainSilence - time.Since(time.Unix(0, renderedAt))
		if remaining > 0 {
			timer := time.NewTimer(remaining)
			select {
			case <-timer.C:
			case <-p.done:
				timer.Stop()
			}
		}
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		_ = p.stdin.Close()
	}
	p.mu.Unlock()
	<-p.done
	p.mu.Lock()
	err := p.waitErr
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("mpv failed: %w: %s", err, strings.TrimSpace(p.stderr.String()))
	}
	return nil
}

func (p *rawAudioPlayer) Cancel() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		if !p.closed {
			p.closed = true
			_ = p.stdin.Close()
		}
		p.mu.Unlock()
		select {
		case <-p.done:
			return
		default:
			interruptProcess(p.command.Process)
		}
		select {
		case <-p.done:
		case <-time.After(750 * time.Millisecond):
			killProcess(p.command.Process)
			<-p.done
		}
	})
}
