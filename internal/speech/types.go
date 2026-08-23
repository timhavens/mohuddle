package speech

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

var ErrPlaybackUnavailable = errors.New("speech playback unavailable")

const (
	DefaultChunkChars   = 3000
	DefaultSegmentChars = 240
	DefaultWorkerNice   = 5
)

type ProviderName string

const (
	ProviderEdge   ProviderName = "edge"
	ProviderKokoro ProviderName = "kokoro"
)

type Mode string

const (
	ModeAll   Mode = "all"
	ModeAgent Mode = "agent"
)

type Config struct {
	Enabled         bool                        `json:"enabled,omitempty"`
	Provider        ProviderName                `json:"provider,omitempty"`
	Mode            Mode                        `json:"mode,omitempty"`
	Agent           chat.Participant            `json:"agent,omitempty"`
	Voices          map[chat.Participant]string `json:"voices,omitempty"`
	AnnounceAgent   bool                        `json:"announce_agent,omitempty"`
	MaxChunkChars   int                         `json:"max_chunk_chars,omitempty"`
	MaxSegmentChars int                         `json:"max_segment_chars,omitempty"`
	PlaybackBinary  string                      `json:"playback_binary,omitempty"`
	PythonBinary    string                      `json:"python_binary,omitempty"`
	ModelPath       string                      `json:"model_path,omitempty"`
	VoicesPath      string                      `json:"voices_path,omitempty"`
	PlayerBinary    string                      `json:"player_binary,omitempty"`
	WorkerNice      *int                        `json:"worker_nice,omitempty"`
}

func (c Config) WithDefaults() Config {
	if c.Provider == "" {
		c.Provider = ProviderEdge
	}
	if c.Mode == "" {
		c.Mode = ModeAll
	}
	if c.MaxChunkChars < 1 {
		c.MaxChunkChars = DefaultChunkChars
	}
	if c.MaxSegmentChars < 1 {
		c.MaxSegmentChars = DefaultSegmentChars
	}
	if c.WorkerNice == nil {
		value := DefaultWorkerNice
		c.WorkerNice = &value
	} else {
		value := *c.WorkerNice
		c.WorkerNice = &value
	}
	c.PlaybackBinary = strings.TrimSpace(c.PlaybackBinary)
	c.PythonBinary = strings.TrimSpace(c.PythonBinary)
	c.ModelPath = strings.TrimSpace(c.ModelPath)
	c.VoicesPath = strings.TrimSpace(c.VoicesPath)
	c.PlayerBinary = strings.TrimSpace(c.PlayerBinary)
	c.Voices = cloneVoices(c.Voices)
	for participant, voice := range c.Voices {
		voice = strings.TrimSpace(voice)
		if !participant.ValidAgent() || voice == "" {
			delete(c.Voices, participant)
			continue
		}
		c.Voices[participant] = voice
	}
	return c
}

func (c Config) Validate() error {
	c = c.WithDefaults()
	if c.Provider != ProviderEdge && c.Provider != ProviderKokoro {
		return fmt.Errorf("invalid speech provider %q", c.Provider)
	}
	if c.Mode != ModeAll && c.Mode != ModeAgent {
		return fmt.Errorf("invalid speech mode %q", c.Mode)
	}
	if c.Mode == ModeAgent && !c.Agent.ValidAgent() {
		return fmt.Errorf("speech agent selection is invalid")
	}
	if c.MaxChunkChars < 1 {
		return fmt.Errorf("speech chunk size must be positive")
	}
	if c.MaxSegmentChars < 1 {
		return fmt.Errorf("speech segment size must be positive")
	}
	if c.WorkerNice == nil || *c.WorkerNice < 0 || *c.WorkerNice > 19 {
		return fmt.Errorf("speech worker nice value must be between 0 and 19")
	}
	for participant, voice := range c.Voices {
		if !participant.ValidAgent() {
			return fmt.Errorf("invalid speech participant %q", participant)
		}
		if err := validateVoice(voice); err != nil {
			return fmt.Errorf("%s voice: %w", participant, err)
		}
	}
	return nil
}

func cloneVoices(source map[chat.Participant]string) map[chat.Participant]string {
	if len(source) == 0 {
		return map[chat.Participant]string{}
	}
	result := make(map[chat.Participant]string, len(source))
	for participant, voice := range source {
		result[participant] = voice
	}
	return result
}

func validateVoice(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("voice name is empty")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("voice name contains invalid characters")
	}
	return nil
}

type Voice struct {
	Name        string
	Description string
}

type Provider interface {
	Validate() error
	Play(context.Context, string, []string) error
	ListVoices(context.Context, string) ([]Voice, error)
	Close() error
}

type EventType string

const (
	EventState    EventType = "state"
	EventStarted  EventType = "started"
	EventFinished EventType = "finished"
	EventError    EventType = "error"
)

type Event struct {
	Type  EventType
	Agent chat.Participant
	Err   error
	State State
}

type State struct {
	Config       Config
	Available    bool
	Unavailable  string
	Speaking     bool
	CurrentAgent chat.Participant
	Queued       int
}

type Controller interface {
	Speak(chat.Participant, string)
	SetEnabled(bool) error
	SetSelection(Mode, chat.Participant) error
	SetVoice(chat.Participant, string) error
	Stop()
	Skip()
	Snapshot() State
	Events() <-chan Event
	ListVoices(context.Context, string) ([]Voice, error)
	Close() error
}
