package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandArguments(t *testing.T) {
	arguments, inArguments := expandArguments([]string{"--voice", "{voice}", "--text={text}"}, "hello world", "voice-one")
	if !inArguments {
		t.Fatal("text placeholder was not detected")
	}
	if got := strings.Join(arguments, "|"); got != "--voice|voice-one|--text=hello world" {
		t.Fatalf("arguments=%q", got)
	}
}

func TestRunCapturesFirstByteAndAudio(t *testing.T) {
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.json")
	contents := `{"version":1,"cases":[{"id":"short","voice_slot":"agent","text":"hello benchmark"}]}`
	if err := os.WriteFile(corpusPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_TTS_SPIKE_HELPER", "1")
	config := configuration{
		Provider: "fake", Binary: os.Args[0],
		Arguments: []string{"-test.run=TestTTSHelperProcess", "--"},
		Voices:    map[string]string{"agent": "test-voice"}, Corpus: corpusPath,
		Runs: 1, Timeout: 2 * time.Second, OutputDir: dir, Extension: "pcm",
	}
	value, failures, err := run(config)
	if err != nil {
		t.Fatal(err)
	}
	if failures != 0 || len(value.Results) != 1 {
		t.Fatalf("failures=%d results=%+v", failures, value.Results)
	}
	result := value.Results[0]
	if result.Error != "" || result.FirstByteMS < 1 || result.AudioBytes != 8 {
		t.Fatalf("result=%+v", result)
	}
	audio, err := os.ReadFile(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "PCM1PCM2" {
		t.Fatalf("audio=%q", audio)
	}
}

func TestTTSHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_TTS_SPIKE_HELPER") != "1" {
		return
	}
	text, err := io.ReadAll(os.Stdin)
	if err != nil || string(text) != "hello benchmark" {
		os.Exit(3)
	}
	_, _ = os.Stdout.Write([]byte("PCM1"))
	time.Sleep(20 * time.Millisecond)
	_, _ = os.Stdout.Write([]byte("PCM2"))
	os.Exit(0)
}

func TestValidateCase(t *testing.T) {
	for _, item := range []testCase{{ID: "../bad", Text: "text"}, {ID: "empty", Text: "  "}, {ID: "negative", Text: "text", Repeat: -1}} {
		if err := validateCase(item); err == nil {
			t.Fatalf("case %+v unexpectedly passed validation", item)
		}
	}
}
