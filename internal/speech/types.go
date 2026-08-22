package speech

import (
	"context"
	"fmt"
	"strings"

	"github.com/timhavens/mohuddle/internal/chat"
)

const DefaultChunkChars = 3000

type Mode string

const (
	ModeAll   Mode = "all"
	ModeAgent Mode = "agent"
)

type Config struct {
	Enabled        bool                        `json:"enabled,omitempty"`
	Mode           Mode                        `json:"mode,omitempty"`
	Agent          chat.Participant            `json:"agent,omitempty"`
	Voices         map[chat.Participant]string `json:"voices,omitempty"`
	AnnounceAgent  bool                        `json:"announce_agent,omitempty"`
	MaxChunkChars  int                         `json:"max_chunk_chars,omitempty"`
	PlaybackBinary string                      `json:"playback_binary,omitempty"`
}

func (c Config) WithDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeAll
	}
	if c.MaxChunkChars < 1 {
		c.MaxChunkChars = DefaultChunkChars
	}
	c.PlaybackBinary = strings.TrimSpace(c.PlaybackBinary)
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
	if c.Mode != ModeAll && c.Mode != ModeAgent {
		return fmt.Errorf("invalid speech mode %q", c.Mode)
	}
	if c.Mode == ModeAgent && !c.Agent.ValidAgent() {
		return fmt.Errorf("speech agent selection is invalid")
	}
	if c.MaxChunkChars < 1 {
		return fmt.Errorf("speech chunk size must be positive")
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
	Play(context.Context, string, string) error
	ListVoices(context.Context, string) ([]Voice, error)
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
