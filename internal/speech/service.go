package speech

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/timhavens/mohuddle/internal/chat"
)

type utterance struct {
	agent    chat.Participant
	voice    string
	segments []string
}

type Service struct {
	provider Provider
	persist  func(Config) error

	mu            sync.Mutex
	config        Config
	available     bool
	unavailable   string
	queue         []utterance
	current       *utterance
	currentCancel context.CancelFunc
	closed        bool
	wake          chan struct{}
	events        chan Event
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func New(config Config, provider Provider, persist func(Config) error) *Service {
	config = config.WithDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		provider: provider, persist: persist, config: config,
		wake: make(chan struct{}, 1), events: make(chan Event, 64), ctx: ctx, cancel: cancel,
	}
	if config.Enabled {
		service.validateProvider()
	}
	service.wg.Add(1)
	go service.run()
	service.emit(EventState, "", nil)
	return service
}

func (s *Service) Events() <-chan Event { return s.events }

func (s *Service) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() State {
	state := State{
		Config: s.config.WithDefaults(), Available: s.available,
		Unavailable: s.unavailable, Queued: len(s.queue),
	}
	if s.current != nil {
		state.Speaking = true
		state.CurrentAgent = s.current.agent
	}
	return state
}

func (s *Service) Speak(participant chat.Participant, text string) {
	if !participant.ValidAgent() {
		return
	}
	s.mu.Lock()
	config := s.config.WithDefaults()
	eligible := !s.closed && config.Enabled && s.available && (config.Mode == ModeAll || config.Agent == participant)
	voice := config.Voices[participant]
	s.mu.Unlock()
	if !eligible || voice == "" {
		return
	}
	normalized := Normalize(text)
	if normalized == "" {
		return
	}
	if config.AnnounceAgent {
		normalized = strings.ToUpper(string(participant[:1])) + string(participant[1:]) + " says. " + normalized
	}
	segments := Segments(normalized, config.MaxSegmentChars)
	if len(segments) == 0 {
		return
	}

	s.mu.Lock()
	current := s.config.WithDefaults()
	eligible = !s.closed && current.Enabled && s.available && (current.Mode == ModeAll || current.Agent == participant)
	voice = current.Voices[participant]
	if !eligible || voice == "" {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, utterance{agent: participant, voice: voice, segments: segments})
	state := s.snapshotLocked()
	s.mu.Unlock()
	s.send(Event{Type: EventState, State: state})
	s.signal()
}

func (s *Service) SetEnabled(enabled bool) error {
	s.mu.Lock()
	s.config.Enabled = enabled
	config := s.config.WithDefaults()
	if !enabled {
		s.stopLocked(true)
	}
	s.mu.Unlock()
	persistErr := s.save(config)
	var validationErr error
	if enabled {
		validationErr = s.validateProvider()
		s.signal()
	}
	s.emit(EventState, "", validationErr)
	if persistErr != nil {
		return persistErr
	}
	return validationErr
}

func (s *Service) SetSelection(mode Mode, participant chat.Participant) error {
	if mode != ModeAll && mode != ModeAgent {
		return fmt.Errorf("invalid speech selection %q", mode)
	}
	if mode == ModeAgent && !participant.ValidAgent() {
		return fmt.Errorf("invalid speech agent %q", participant)
	}
	s.mu.Lock()
	s.config.Mode = mode
	s.config.Agent = participant
	s.config.Enabled = true
	s.stopLocked(true)
	config := s.config.WithDefaults()
	s.mu.Unlock()
	persistErr := s.save(config)
	validationErr := s.validateProvider()
	s.emit(EventState, "", validationErr)
	if persistErr != nil {
		return persistErr
	}
	return validationErr
}

func (s *Service) SetVoice(participant chat.Participant, voice string) error {
	if !participant.ValidAgent() {
		return fmt.Errorf("invalid speech agent %q", participant)
	}
	voice = strings.TrimSpace(voice)
	if voice != "" {
		if err := validateVoice(voice); err != nil {
			return err
		}
	}
	s.mu.Lock()
	voices := cloneVoices(s.config.Voices)
	if voice == "" {
		delete(voices, participant)
	} else {
		voices[participant] = voice
	}
	s.config.Voices = voices
	config := s.config.WithDefaults()
	s.mu.Unlock()
	err := s.save(config)
	s.emit(EventState, "", nil)
	return err
}

func (s *Service) ListVoices(ctx context.Context, filter string) ([]Voice, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("speech provider is unavailable")
	}
	return s.provider.ListVoices(ctx, filter)
}

func (s *Service) Stop() {
	s.mu.Lock()
	s.stopLocked(true)
	state := s.snapshotLocked()
	s.mu.Unlock()
	s.send(Event{Type: EventState, State: state})
}

func (s *Service) Skip() {
	s.mu.Lock()
	if s.currentCancel != nil {
		s.currentCancel()
	}
	state := s.snapshotLocked()
	s.mu.Unlock()
	s.send(Event{Type: EventState, State: state})
}

func (s *Service) stopLocked(clearQueue bool) {
	if s.currentCancel != nil {
		s.currentCancel()
	}
	if clearQueue {
		s.queue = nil
	}
}

func (s *Service) validateProvider() error {
	var err error
	s.mu.Lock()
	config := s.config.WithDefaults()
	s.mu.Unlock()
	if configErr := config.Validate(); configErr != nil {
		err = configErr
	} else if s.provider == nil {
		err = fmt.Errorf("speech provider is unavailable")
	} else {
		err = s.provider.Validate()
	}
	s.mu.Lock()
	s.available = err == nil
	s.unavailable = ""
	if err != nil {
		s.unavailable = err.Error()
	}
	s.mu.Unlock()
	return err
}

func (s *Service) save(config Config) error {
	if s.persist == nil {
		return nil
	}
	if err := s.persist(config); err != nil {
		return fmt.Errorf("save speech settings: %w", err)
	}
	return nil
}

func (s *Service) run() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			item, ctx := s.next()
			if item == nil {
				break
			}
			s.emit(EventStarted, item.agent, nil)
			failed := false
			if err := s.provider.Play(ctx, item.voice, item.segments); err != nil {
				if ctx.Err() == nil && s.ctx.Err() == nil {
					if errors.Is(err, ErrPlaybackUnavailable) {
						s.markUnavailable(err)
					}
					s.emit(EventError, item.agent, err)
					failed = true
				}
			}
			s.finishCurrent()
			if !failed && ctx.Err() == nil {
				s.emit(EventFinished, item.agent, nil)
			}
		}
	}
}

func (s *Service) markUnavailable(err error) {
	s.mu.Lock()
	s.available = false
	s.unavailable = err.Error()
	s.queue = nil
	s.mu.Unlock()
}

func (s *Service) next() (*utterance, context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.config.Enabled || !s.available || len(s.queue) == 0 {
		return nil, nil
	}
	item := s.queue[0]
	s.queue = s.queue[1:]
	ctx, cancel := context.WithCancel(s.ctx)
	s.current = &item
	s.currentCancel = cancel
	s.sendLocked(Event{Type: EventState, State: s.snapshotLocked()})
	return &item, ctx
}

func (s *Service) finishCurrent() {
	s.mu.Lock()
	if s.currentCancel != nil {
		s.currentCancel()
	}
	s.currentCancel = nil
	s.current = nil
	state := s.snapshotLocked()
	more := len(s.queue) > 0
	s.mu.Unlock()
	s.send(Event{Type: EventState, State: state})
	if more {
		signalChannel(s.wake)
	}
}

func (s *Service) signal() { signalChannel(s.wake) }

func signalChannel(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

func (s *Service) emit(kind EventType, participant chat.Participant, err error) {
	s.mu.Lock()
	event := Event{Type: kind, Agent: participant, Err: err, State: s.snapshotLocked()}
	s.mu.Unlock()
	s.send(event)
}

func (s *Service) send(event Event) {
	select {
	case s.events <- event:
	default:
	}
}

func (s *Service) sendLocked(event Event) {
	select {
	case s.events <- event:
	default:
	}
}

func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.stopLocked(true)
	s.mu.Unlock()
	s.cancel()
	s.signal()
	s.wg.Wait()
	var providerErr error
	if s.provider != nil {
		providerErr = s.provider.Close()
	}
	close(s.events)
	return providerErr
}
