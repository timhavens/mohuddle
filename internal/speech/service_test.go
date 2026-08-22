package speech

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/chat"
)

type playCall struct {
	voice string
	text  string
}

type fakeProvider struct {
	validateErr error
	plays       chan playCall
	release     chan struct{}
	failNext    bool
	mu          sync.Mutex
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{plays: make(chan playCall, 16), release: make(chan struct{}, 16)}
}

func (f *fakeProvider) Validate() error { return f.validateErr }
func (f *fakeProvider) ListVoices(context.Context, string) ([]Voice, error) {
	return []Voice{{Name: "voice-one"}}, nil
}
func (f *fakeProvider) Play(ctx context.Context, voice, text string) error {
	f.plays <- playCall{voice: voice, text: text}
	f.mu.Lock()
	fail := f.failNext
	f.failNext = false
	f.mu.Unlock()
	if fail {
		return errors.New("provider failed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.release:
		return nil
	}
}

func TestServiceSerializesMessagesAndUsesPerAgentVoices(t *testing.T) {
	provider := newFakeProvider()
	service := New(Config{
		Enabled: true, Mode: ModeAll,
		Voices: map[chat.Participant]string{chat.Codex: "codex-voice", chat.Claude: "claude-voice"},
	}, provider, nil)
	defer service.Close()
	service.Speak(chat.Codex, "first response")
	service.Speak(chat.Claude, "second response")
	first := receivePlay(t, provider.plays)
	if first.voice != "codex-voice" || first.text != "first response" {
		t.Fatalf("first=%+v", first)
	}
	select {
	case unexpected := <-provider.plays:
		t.Fatalf("overlapping playback: %+v", unexpected)
	case <-time.After(40 * time.Millisecond):
	}
	provider.release <- struct{}{}
	second := receivePlay(t, provider.plays)
	if second.voice != "claude-voice" || second.text != "second response" {
		t.Fatalf("second=%+v", second)
	}
	provider.release <- struct{}{}
}

func TestServiceSpeaksEveryLongResponseChunk(t *testing.T) {
	provider := newFakeProvider()
	service := New(Config{
		Enabled: true, Mode: ModeAll, MaxChunkChars: 22,
		Voices: map[chat.Participant]string{chat.Codex: "voice"},
	}, provider, nil)
	defer service.Close()
	input := "First sentence. Second sentence. Third sentence."
	service.Speak(chat.Codex, input)
	want := Chunk(Normalize(input), 22)
	var got []string
	for range want {
		call := receivePlay(t, provider.plays)
		got = append(got, call.text)
		provider.release <- struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("chunks=%q want=%q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("chunks=%q want=%q", got, want)
		}
	}
}

func TestServiceStopClearsQueueAndSkipContinues(t *testing.T) {
	provider := newFakeProvider()
	service := New(Config{
		Enabled: true, Mode: ModeAll,
		Voices: map[chat.Participant]string{chat.Codex: "voice", chat.Claude: "voice"},
	}, provider, nil)
	defer service.Close()

	service.Speak(chat.Codex, "skip me")
	service.Speak(chat.Claude, "play me")
	_ = receivePlay(t, provider.plays)
	service.Skip()
	next := receivePlay(t, provider.plays)
	if next.text != "play me" {
		t.Fatalf("after skip=%+v", next)
	}

	service.Speak(chat.Codex, "queued")
	service.Stop()
	provider.release <- struct{}{}
	time.Sleep(30 * time.Millisecond)
	select {
	case unexpected := <-provider.plays:
		t.Fatalf("stop did not clear queue: %+v", unexpected)
	default:
	}
	if state := service.Snapshot(); !state.Config.Enabled || state.Queued != 0 {
		t.Fatalf("state after stop=%+v", state)
	}
}

func TestServiceSkipsUnmappedAgentsAndPersistsControls(t *testing.T) {
	provider := newFakeProvider()
	var persisted Config
	service := New(Config{}, provider, func(config Config) error {
		persisted = config
		return nil
	})
	defer service.Close()
	if err := service.SetVoice(chat.Codex, "voice"); err != nil {
		t.Fatal(err)
	}
	if err := service.SetSelection(ModeAgent, chat.Codex); err != nil {
		t.Fatal(err)
	}
	service.Speak(chat.Claude, "not selected or mapped")
	select {
	case unexpected := <-provider.plays:
		t.Fatalf("unmapped agent played: %+v", unexpected)
	case <-time.After(30 * time.Millisecond):
	}
	if !persisted.Enabled || persisted.Mode != ModeAgent || persisted.Agent != chat.Codex || persisted.Voices[chat.Codex] != "voice" {
		t.Fatalf("persisted=%+v", persisted)
	}
}

func TestServiceFailureDoesNotBlockNextMessage(t *testing.T) {
	provider := newFakeProvider()
	provider.failNext = true
	service := New(Config{
		Enabled: true, Mode: ModeAll,
		Voices: map[chat.Participant]string{chat.Codex: "voice", chat.Claude: "voice"},
	}, provider, nil)
	defer service.Close()
	service.Speak(chat.Codex, "fails")
	service.Speak(chat.Claude, "continues")
	_ = receivePlay(t, provider.plays)
	next := receivePlay(t, provider.plays)
	if next.text != "continues" {
		t.Fatalf("next=%+v", next)
	}
	provider.release <- struct{}{}
}

func TestServiceRejectsInvalidPersistedSelectionWhenEnabled(t *testing.T) {
	provider := newFakeProvider()
	service := New(Config{Enabled: true, Mode: ModeAgent}, provider, nil)
	defer service.Close()
	state := service.Snapshot()
	if state.Available || state.Unavailable == "" {
		t.Fatalf("invalid selection state=%+v", state)
	}
}

func receivePlay(t *testing.T, plays <-chan playCall) playCall {
	t.Helper()
	select {
	case call := <-plays:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for playback")
		return playCall{}
	}
}
