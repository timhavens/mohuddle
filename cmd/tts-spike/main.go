// tts-spike benchmarks an external TTS command without integrating it into
// MoHuddle's live speech path. It is intentionally provider-neutral: candidate
// adapters must emit audio bytes on stdout and accept text either on stdin or
// through an explicit {text} argument placeholder.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type corpus struct {
	Version int        `json:"version"`
	Cases   []testCase `json:"cases"`
}

type testCase struct {
	ID        string `json:"id"`
	VoiceSlot string `json:"voice_slot,omitempty"`
	Text      string `json:"text"`
	Repeat    int    `json:"repeat,omitempty"`
}

func (c testCase) expandedText() string {
	repeat := c.Repeat
	if repeat < 1 {
		repeat = 1
	}
	parts := make([]string, repeat)
	for index := range parts {
		parts[index] = strings.TrimSpace(c.Text)
	}
	return strings.Join(parts, " ")
}

type configuration struct {
	Provider  string
	Binary    string
	Arguments []string
	Voices    map[string]string
	Corpus    string
	Runs      int
	Timeout   time.Duration
	OutputDir string
	Extension string
	Report    string
}

type report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Provider    string    `json:"provider"`
	Binary      string    `json:"binary"`
	Arguments   []string  `json:"arguments"`
	Corpus      string    `json:"corpus"`
	ProcessMode string    `json:"process_mode"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	Results     []result  `json:"results"`
}

type result struct {
	CaseID       string `json:"case_id"`
	VoiceSlot    string `json:"voice_slot,omitempty"`
	Voice        string `json:"voice,omitempty"`
	Run          int    `json:"run"`
	Characters   int    `json:"characters"`
	AudioBytes   int64  `json:"audio_bytes"`
	FirstByteMS  int64  `json:"first_audio_byte_ms,omitempty"`
	TotalMS      int64  `json:"total_ms"`
	UserCPUMS    int64  `json:"user_cpu_ms,omitempty"`
	SystemCPUMS  int64  `json:"system_cpu_ms,omitempty"`
	PeakRSSBytes int64  `json:"peak_rss_bytes,omitempty"`
	Output       string `json:"output,omitempty"`
	Error        string `json:"error,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
}

func main() {
	config, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "tts-spike:", err)
		os.Exit(2)
	}
	value, failures, err := run(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tts-spike:", err)
		os.Exit(1)
	}
	if err := writeReport(config.Report, value); err != nil {
		fmt.Fprintln(os.Stderr, "tts-spike:", err)
		os.Exit(1)
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "tts-spike: %d benchmark run(s) failed\n", failures)
		os.Exit(1)
	}
}

func parseFlags(arguments []string) (configuration, error) {
	set := flag.NewFlagSet("tts-spike", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	var args repeatedFlag
	var voices repeatedFlag
	config := configuration{}
	set.StringVar(&config.Provider, "provider", "command", "provider label used in the report")
	set.StringVar(&config.Binary, "binary", "", "external provider adapter executable")
	set.Var(&args, "arg", "adapter argument; repeat as needed; supports {text} and {voice}")
	set.Var(&voices, "voice", "voice mapping in SLOT=NAME form; repeat as needed")
	set.StringVar(&config.Corpus, "corpus", "cmd/tts-spike/testdata/corpus.json", "benchmark corpus JSON")
	set.IntVar(&config.Runs, "runs", 1, "runs per corpus case")
	set.DurationVar(&config.Timeout, "timeout", 2*time.Minute, "timeout per run")
	set.StringVar(&config.OutputDir, "output-dir", "", "optional directory for captured audio")
	set.StringVar(&config.Extension, "extension", "audio", "captured audio filename extension")
	set.StringVar(&config.Report, "report", "-", "JSON report path, or - for stdout")
	if err := set.Parse(arguments); err != nil {
		return configuration{}, err
	}
	if config.Binary == "" {
		return configuration{}, errors.New("--binary is required")
	}
	if config.Runs < 1 {
		return configuration{}, errors.New("--runs must be positive")
	}
	if config.Timeout <= 0 {
		return configuration{}, errors.New("--timeout must be positive")
	}
	if strings.ContainsAny(config.Extension, `/\\`) || strings.Trim(config.Extension, ". ") == "" {
		return configuration{}, errors.New("--extension must be a simple filename extension")
	}
	config.Arguments = append([]string(nil), args...)
	config.Voices = make(map[string]string, len(voices))
	for _, mapping := range voices {
		slot, voice, found := strings.Cut(mapping, "=")
		slot, voice = strings.TrimSpace(slot), strings.TrimSpace(voice)
		if !found || slot == "" || voice == "" {
			return configuration{}, fmt.Errorf("invalid --voice %q; want SLOT=NAME", mapping)
		}
		config.Voices[slot] = voice
	}
	return config, nil
}

func run(config configuration) (report, int, error) {
	contents, err := os.ReadFile(config.Corpus)
	if err != nil {
		return report{}, 0, fmt.Errorf("read corpus: %w", err)
	}
	var suite corpus
	if err := json.Unmarshal(contents, &suite); err != nil {
		return report{}, 0, fmt.Errorf("decode corpus: %w", err)
	}
	if suite.Version != 1 || len(suite.Cases) == 0 {
		return report{}, 0, fmt.Errorf("corpus must use version 1 and contain at least one case")
	}
	if config.OutputDir != "" {
		if err := os.MkdirAll(config.OutputDir, 0o700); err != nil {
			return report{}, 0, fmt.Errorf("create output directory: %w", err)
		}
	}

	value := report{
		GeneratedAt: time.Now().UTC(), Provider: config.Provider, Binary: config.Binary,
		Arguments: append([]string(nil), config.Arguments...), Corpus: config.Corpus,
		ProcessMode: "one_process_per_case", OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
	failures := 0
	for _, item := range suite.Cases {
		if err := validateCase(item); err != nil {
			return report{}, 0, err
		}
		voice := config.Voices[item.VoiceSlot]
		if item.VoiceSlot != "" && voice == "" {
			return report{}, 0, fmt.Errorf("case %q requires --voice %s=NAME", item.ID, item.VoiceSlot)
		}
		for iteration := 1; iteration <= config.Runs; iteration++ {
			measured := runCase(config, item, voice, iteration)
			if measured.Error != "" {
				failures++
			}
			value.Results = append(value.Results, measured)
		}
	}
	return value, failures, nil
}

func validateCase(item testCase) error {
	if item.ID == "" || strings.ContainsAny(item.ID, `/\\`) || item.ID == "." || item.ID == ".." {
		return fmt.Errorf("invalid corpus case ID %q", item.ID)
	}
	if strings.TrimSpace(item.Text) == "" {
		return fmt.Errorf("corpus case %q has no text", item.ID)
	}
	if item.Repeat < 0 {
		return fmt.Errorf("corpus case %q has a negative repeat count", item.ID)
	}
	return nil
}

func runCase(config configuration, item testCase, voice string, iteration int) result {
	text := item.expandedText()
	arguments, textInArguments := expandArguments(config.Arguments, text, voice)
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()
	command := exec.CommandContext(ctx, config.Binary, arguments...)
	if !textInArguments {
		command.Stdin = strings.NewReader(text)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return failedResult(item, voice, iteration, text, err)
	}
	var stderr cappedBuffer
	command.Stderr = &stderr

	measured := result{CaseID: item.ID, VoiceSlot: item.VoiceSlot, Voice: voice, Run: iteration, Characters: len([]rune(text))}
	var destination io.WriteCloser = nopWriteCloser{Writer: io.Discard}
	if config.OutputDir != "" {
		path := filepath.Join(config.OutputDir, fmt.Sprintf("%s-run-%02d.%s", item.ID, iteration, strings.TrimPrefix(config.Extension, ".")))
		file, createErr := os.Create(path)
		if createErr != nil {
			return failedResult(item, voice, iteration, text, createErr)
		}
		destination = file
		measured.Output = path
	}
	defer destination.Close()

	started := time.Now()
	if err := command.Start(); err != nil {
		measured.Error = err.Error()
		return measured
	}
	rssDone := make(chan struct{})
	peakRSS := samplePeakRSS(command.Process.Pid, rssDone)
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := stdout.Read(buffer)
		if count > 0 {
			if measured.FirstByteMS == 0 {
				measured.FirstByteMS = max(1, time.Since(started).Milliseconds())
			}
			written, writeErr := destination.Write(buffer[:count])
			measured.AudioBytes += int64(written)
			if writeErr != nil {
				measured.Error = writeErr.Error()
				cancel()
				break
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && measured.Error == "" {
				measured.Error = readErr.Error()
			}
			break
		}
	}
	waitErr := command.Wait()
	close(rssDone)
	measured.PeakRSSBytes = <-peakRSS
	measured.TotalMS = time.Since(started).Milliseconds()
	if state := command.ProcessState; state != nil {
		measured.UserCPUMS = state.UserTime().Milliseconds()
		measured.SystemCPUMS = state.SystemTime().Milliseconds()
	}
	if ctx.Err() == context.DeadlineExceeded {
		measured.Error = "timeout after " + config.Timeout.String()
	} else if waitErr != nil && measured.Error == "" {
		measured.Error = waitErr.Error()
	}
	measured.Stderr = stderr.String()
	return measured
}

func failedResult(item testCase, voice string, iteration int, text string, err error) result {
	return result{CaseID: item.ID, VoiceSlot: item.VoiceSlot, Voice: voice, Run: iteration, Characters: len([]rune(text)), Error: err.Error()}
}

func expandArguments(arguments []string, text, voice string) ([]string, bool) {
	expanded := make([]string, len(arguments))
	textInArguments := false
	for index, argument := range arguments {
		if strings.Contains(argument, "{text}") {
			textInArguments = true
		}
		argument = strings.ReplaceAll(argument, "{text}", text)
		argument = strings.ReplaceAll(argument, "{voice}", voice)
		expanded[index] = argument
	}
	return expanded, textInArguments
}

func writeReport(path string, value report) error {
	var target io.Writer = os.Stdout
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.Create(path)
		if err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		defer file.Close()
		target = file
	}
	encoder := json.NewEncoder(target)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 64 * 1024
	remaining := limit - b.buf.Len()
	if remaining > 0 {
		_, _ = b.buf.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func samplePeakRSS(pid int, done <-chan struct{}) <-chan int64 {
	result := make(chan int64, 1)
	go func() {
		defer close(result)
		if runtime.GOOS != "linux" {
			result <- 0
			return
		}
		var peak int64
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if current := readRSS(pid); current > peak {
				peak = current
			}
			select {
			case <-done:
				result <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	return result
}

func readRSS(pid int) int64 {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kilobytes, _ := strconv.ParseInt(fields[1], 10, 64)
		return kilobytes * 1024
	}
	return 0
}
