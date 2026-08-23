package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/speech"
)

func main() {
	var (
		voice      string
		text       string
		list       bool
		filter     string
		python     string
		model      string
		voices     string
		player     string
		segment    int
		repeat     int
		pause      time.Duration
		configure  bool
		configPath string
	)
	flag.StringVar(&voice, "voice", "am_adam", "Kokoro voice name")
	flag.StringVar(&text, "text", "MoHuddle local speech is ready.", "text to speak")
	flag.BoolVar(&list, "list", false, "list installed Kokoro voices")
	flag.StringVar(&filter, "filter", "", "filter listed voices")
	flag.StringVar(&python, "python", "", "Kokoro virtual-environment Python")
	flag.StringVar(&model, "model", "", "Kokoro ONNX model")
	flag.StringVar(&voices, "voices", "", "Kokoro voice bank")
	flag.StringVar(&player, "player", "", "mpv executable")
	flag.IntVar(&segment, "segment-chars", speech.DefaultSegmentChars, "maximum synthesis request size")
	flag.IntVar(&repeat, "repeat", 1, "number of utterances to play through the same worker and player")
	flag.DurationVar(&pause, "pause", 0, "pause between repeated utterances")
	flag.BoolVar(&configure, "configure", false, "select Kokoro and install the tested per-agent voice preset")
	flag.StringVar(&configPath, "config", "", "personal settings file used with --configure")
	flag.Parse()
	if configure {
		configureSpeech(configPath)
		return
	}

	nice := speech.DefaultWorkerNice
	config := speech.Config{
		Provider: speech.ProviderKokoro, PythonBinary: python, ModelPath: model,
		VoicesPath: voices, PlayerBinary: player, WorkerNice: &nice,
	}
	provider := speech.NewKokoroProvider(config)
	defer provider.Close()
	if err := provider.Validate(); err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if list {
		available, err := provider.ListVoices(ctx, filter)
		if err != nil {
			fail(err)
		}
		for _, item := range available {
			fmt.Println(item.Name)
		}
		return
	}
	segments := speech.Segments(speech.Normalize(text), segment)
	if len(segments) == 0 {
		fail(fmt.Errorf("no speakable text remains after normalization"))
	}
	if repeat < 1 {
		fail(fmt.Errorf("repeat must be positive"))
	}
	for run := 0; run < repeat; run++ {
		if run > 0 && pause > 0 {
			time.Sleep(pause)
		}
		if err := provider.Play(ctx, voice, segments); err != nil {
			fail(err)
		}
	}
}

func configureSpeech(path string) {
	store, err := appsettings.Open(path)
	if err != nil {
		fail(err)
	}
	config := store.SpeechSettings()
	config.Enabled = false
	config.Provider = speech.ProviderKokoro
	config.Voices = map[chat.Participant]string{
		chat.Codex: "am_adam", chat.Claude: "af_sarah",
		chat.Agy: "am_michael", chat.Copilot: "af_nova",
	}
	config.MaxSegmentChars = speech.DefaultSegmentChars
	if err := store.SetSpeechSettings(config); err != nil {
		fail(err)
	}
	fmt.Println("Configured Kokoro speech in", store.Path())
	fmt.Println("Speech remains off; restart MoHuddle and run /speak all when ready.")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tts-smoke:", err)
	os.Exit(1)
}
