package speech

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

type EdgeProvider struct {
	binary string
	mu     sync.Mutex
	paths  edgePaths
}

type edgePaths struct {
	playback string
	edgeTTS  string
	mpv      string
}

func NewEdgeProvider(binary string) *EdgeProvider {
	return &EdgeProvider{binary: strings.TrimSpace(binary)}
}

func (p *EdgeProvider) Validate() error {
	paths, err := p.resolve()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.paths = paths
	p.mu.Unlock()
	return nil
}

func (p *EdgeProvider) Play(ctx context.Context, voice string, segments []string) error {
	if err := validateVoice(voice); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(segments, " "))
	if text == "" {
		return nil
	}
	paths, err := p.currentPaths()
	if err != nil {
		return err
	}
	return runPlayback(ctx, paths.playback, "--voice", voice, "--text", text)
}

func (p *EdgeProvider) Close() error { return nil }

func (p *EdgeProvider) ListVoices(ctx context.Context, filter string) ([]Voice, error) {
	paths, err := p.currentPaths()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, paths.edgeTTS, "--list-voices")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list Edge voices: %w: %s", err, strings.TrimSpace(string(output)))
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	var voices []Voice
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "Name" || strings.HasPrefix(fields[0], "---") {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(line), filter) {
			continue
		}
		voices = append(voices, Voice{Name: fields[0], Description: strings.Join(fields[1:], " ")})
	}
	sort.Slice(voices, func(i, j int) bool { return voices[i].Name < voices[j].Name })
	return voices, nil
}

func (p *EdgeProvider) currentPaths() (edgePaths, error) {
	p.mu.Lock()
	paths := p.paths
	p.mu.Unlock()
	if paths.playback != "" && paths.edgeTTS != "" && paths.mpv != "" {
		return paths, nil
	}
	if err := p.Validate(); err != nil {
		return edgePaths{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paths, nil
}

func (p *EdgeProvider) resolve() (edgePaths, error) {
	playback := p.binary
	if playback == "" {
		playback = "edge-playback"
	}
	resolvedPlayback, err := resolveExecutable(playback)
	if err != nil && p.binary == "" {
		resolvedPlayback, err = resolveUserLocalExecutable("edge-playback")
	}
	if err != nil {
		return edgePaths{}, fmt.Errorf("edge-playback not available: %w", err)
	}
	edgeTTS, err := resolveExecutable("edge-tts")
	if err != nil {
		edgeTTS, err = resolveUserLocalExecutable("edge-tts")
	}
	if err != nil {
		return edgePaths{}, fmt.Errorf("edge-tts not available: %w", err)
	}
	mpv, err := resolveExecutable("mpv")
	if err != nil {
		return edgePaths{}, fmt.Errorf("mpv not available: %w", err)
	}
	return edgePaths{playback: resolvedPlayback, edgeTTS: edgeTTS, mpv: mpv}, nil
}

func resolveUserLocalExecutable(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return resolveExecutable(filepath.Join(home, ".local", "bin", name))
}

func resolveExecutable(value string) (string, error) {
	if strings.ContainsRune(value, filepath.Separator) {
		info, err := os.Stat(value)
		if err != nil {
			return "", err
		}
		if info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
			return "", fmt.Errorf("%s is not executable", value)
		}
		return filepath.Abs(value)
	}
	return exec.LookPath(value)
}
