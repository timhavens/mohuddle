// Package events provides a bounded, in-memory replay hub for sanitized remote
// API events. Event history is intentionally scoped to one process boot; durable
// transcript recovery remains keyed by Cursor.MessageSequence.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/timhavens/mohuddle/internal/api"
)

const (
	defaultMaxRecords = 4096
	defaultMaxBytes   = 8 << 20
)

var ErrClosed = errors.New("event replay hub is closed")

// Cursor is an opaque resume position. EventSequence is valid only for BootID;
// MessageSequence is the durable transcript high-water observed at that point.
type Cursor struct {
	BootID          string `json:"boot_id"`
	EventSequence   uint64 `json:"event_sequence"`
	MessageSequence uint64 `json:"message_sequence"`
}

func (c Cursor) IsZero() bool {
	return c.BootID == "" && c.EventSequence == 0 && c.MessageSequence == 0
}

type GapReason string

const (
	GapBootMismatch       GapReason = "boot_mismatch"
	GapCursorExpired      GapReason = "cursor_expired"
	GapSubscriberOverflow GapReason = "subscriber_overflow"
	GapUpstreamOverflow   GapReason = "upstream_overflow"
)

// Gap tells a consumer that transient events may be missing. Current is the
// safe reset point; the consumer should recover durable messages after its own
// MessageSequence, refresh its snapshot, then continue after Current.
type Gap struct {
	Reason          GapReason `json:"reason"`
	Requested       Cursor    `json:"requested"`
	OldestAvailable Cursor    `json:"oldest_available"`
	Current         Cursor    `json:"current"`
}

type Record struct {
	Cursor Cursor    `json:"cursor"`
	Event  api.Event `json:"event"`
}

// Delivery is exactly one replay record or one structured gap.
type Delivery struct {
	Record *Record `json:"record,omitempty"`
	Gap    *Gap    `json:"gap,omitempty"`
}

type Options struct {
	// BootID is primarily useful to deterministic tests. A random ID is generated
	// when it is empty.
	BootID                 string
	InitialMessageSequence uint64
	MaxRecords             int
	MaxBytes               int
}

type Hub struct {
	mu sync.Mutex

	bootID          string
	eventSequence   uint64
	messageSequence uint64
	maxRecords      int
	maxBytes        int
	recordBytes     int
	records         []retainedRecord

	nextSubscriber uint64
	subscribers    map[uint64]*subscriber
	closed         bool
	wg             sync.WaitGroup
}

type retainedRecord struct {
	record Record
	data   []byte
	bytes  int
}

func New(options Options) (*Hub, error) {
	bootID := options.BootID
	if bootID == "" {
		var err error
		bootID, err = newBootID()
		if err != nil {
			return nil, err
		}
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = defaultMaxRecords
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.MaxRecords < 1 || options.MaxBytes < 1 {
		return nil, fmt.Errorf("event replay limits must be positive")
	}
	return &Hub{
		bootID: bootID, messageSequence: options.InitialMessageSequence,
		maxRecords: options.MaxRecords, maxBytes: options.MaxBytes,
		subscribers: make(map[uint64]*subscriber),
	}, nil
}

func newBootID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate event boot id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (h *Hub) Current() Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.currentLocked()
}

func (h *Hub) currentLocked() Cursor {
	return Cursor{BootID: h.bootID, EventSequence: h.eventSequence, MessageSequence: h.messageSequence}
}

// Publish deep-copies an already-sanitized API event, assigns one stable cursor,
// retains it within both limits, and fans the same value out to every subscriber.
// Publishing never waits for a subscriber.
func (h *Hub) Publish(event api.Event) (Cursor, error) {
	eventData, err := json.Marshal(event)
	if err != nil {
		return Cursor{}, fmt.Errorf("encode remote event: %w", err)
	}
	var copied api.Event
	if err := json.Unmarshal(eventData, &copied); err != nil {
		return Cursor{}, fmt.Errorf("copy remote event: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return Cursor{}, ErrClosed
	}
	h.eventSequence++
	if copied.Payload.Message != nil && copied.Payload.Message.Sequence > h.messageSequence {
		h.messageSequence = copied.Payload.Message.Sequence
	}
	cursor := h.currentLocked()
	record := Record{Cursor: cursor, Event: copied}
	recordData, err := json.Marshal(record)
	if err != nil {
		return Cursor{}, fmt.Errorf("measure remote event: %w", err)
	}
	h.records = append(h.records, retainedRecord{record: record, data: append([]byte(nil), recordData...), bytes: len(recordData)})
	h.recordBytes += len(recordData)
	h.trimLocked()
	oldest := h.oldestLocked()
	for _, current := range h.subscribers {
		current.enqueueRecord(decodeRetainedRecord(recordData), len(recordData), oldest, cursor)
	}
	return cursor, nil
}

// Invalidate rotates the transient event epoch and delivers a reserved control
// gap to every subscriber. Durable message recovery continues from each
// subscriber's last applied message sequence.
func (h *Hub) Invalidate(reason GapReason) (Cursor, error) {
	if reason == "" {
		return Cursor{}, fmt.Errorf("event gap reason is required")
	}
	bootID, err := newBootID()
	if err != nil {
		return Cursor{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return Cursor{}, ErrClosed
	}
	h.bootID = bootID
	h.eventSequence = 0
	h.recordBytes = 0
	for index := range h.records {
		h.records[index] = retainedRecord{}
	}
	h.records = nil
	current := h.currentLocked()
	for _, subscriber := range h.subscribers {
		subscriber.enqueueGap(reason, current, current)
	}
	return current, nil
}

func (h *Hub) trimLocked() {
	for len(h.records) > 0 && (len(h.records) > h.maxRecords || h.recordBytes > h.maxBytes) {
		h.recordBytes -= h.records[0].bytes
		h.records[0] = retainedRecord{}
		h.records = h.records[1:]
	}
}

func (h *Hub) oldestLocked() Cursor {
	if len(h.records) > 0 {
		return h.records[0].record.Cursor
	}
	return h.currentLocked()
}

// Subscription contains a replay cut and an independent live stream. The
// subscriber is registered before Subscribe returns, so events published after
// Current are queued while the caller applies Replay.
type Subscription struct {
	Replay  []Record
	Current Cursor
	Gap     *Gap
	Events  <-chan Delivery

	cancel func()
	once   sync.Once
}

func (s *Subscription) Cancel() {
	if s == nil || s.cancel == nil {
		return
	}
	s.once.Do(s.cancel)
}

// Subscribe atomically takes a replay cut and registers a live subscriber.
// A zero cursor means a new consumer and starts at the current cut without a
// replay. A nonzero cursor must belong to this boot and remain in the replay
// window. Capacity bounds the subscriber's pending live deliveries.
func (h *Hub) Subscribe(after Cursor, capacity int) (*Subscription, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("subscriber capacity must be positive")
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	current := h.currentLocked()
	oldest := h.oldestLocked()
	replay, gap := h.replayLocked(after, oldest, current)
	h.nextSubscriber++
	id := h.nextSubscriber
	sub := newSubscriber(capacity, h.maxBytes, current)
	h.subscribers[id] = sub
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		sub.run()
	}()
	h.mu.Unlock()

	return &Subscription{
		Replay: replay, Current: current, Gap: gap, Events: sub.output,
		cancel: func() { h.cancel(id) },
	}, nil
}

func (h *Hub) replayLocked(after, oldest, current Cursor) ([]Record, *Gap) {
	if after.IsZero() {
		return nil, nil
	}
	if after.BootID != h.bootID {
		return nil, &Gap{Reason: GapBootMismatch, Requested: after, OldestAvailable: oldest, Current: current}
	}
	if after.EventSequence > current.EventSequence {
		return nil, &Gap{Reason: GapCursorExpired, Requested: after, OldestAvailable: oldest, Current: current}
	}
	if after.EventSequence < current.EventSequence {
		if len(h.records) == 0 || after.EventSequence+1 < oldest.EventSequence {
			return nil, &Gap{Reason: GapCursorExpired, Requested: after, OldestAvailable: oldest, Current: current}
		}
	}
	replay := make([]Record, 0, len(h.records))
	for _, value := range h.records {
		if value.record.Cursor.EventSequence > after.EventSequence {
			replay = append(replay, decodeRetainedRecord(value.data))
		}
	}
	return replay, nil
}

// data was produced from Record immediately before retention, so decoding it
// cannot fail. Decoding for each consumer prevents one subscriber from mutating
// the retained event or another subscriber's remote-sanitized value.
func decodeRetainedRecord(data []byte) Record {
	var value Record
	_ = json.Unmarshal(data, &value)
	return value
}

func (h *Hub) cancel(id uint64) {
	h.mu.Lock()
	current := h.subscribers[id]
	delete(h.subscribers, id)
	h.mu.Unlock()
	if current != nil {
		current.close()
	}
}

func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	values := make([]*subscriber, 0, len(h.subscribers))
	for id, current := range h.subscribers {
		values = append(values, current)
		delete(h.subscribers, id)
	}
	h.mu.Unlock()
	for _, current := range values {
		current.close()
	}
	h.wg.Wait()
}

type subscriber struct {
	mu           sync.Mutex
	capacity     int
	maxBytes     int
	pending      []queuedDelivery
	pendingBytes int
	delivered    Cursor
	notify       chan struct{}
	done         chan struct{}
	output       chan Delivery
	closed       bool
	closeOnce    sync.Once
}

type queuedDelivery struct {
	delivery Delivery
	bytes    int
}

func newSubscriber(capacity, maxBytes int, current Cursor) *subscriber {
	return &subscriber{
		capacity: capacity, maxBytes: maxBytes, delivered: current,
		notify: make(chan struct{}, 1), done: make(chan struct{}), output: make(chan Delivery),
	}
}

func (s *subscriber) enqueueRecord(record Record, recordBytes int, oldest, current Cursor) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if len(s.pending) >= s.capacity || s.pendingBytes+recordBytes > s.maxBytes {
		s.replaceWithGapLocked(GapSubscriberOverflow, oldest, current)
	} else {
		copy := record
		s.pending = append(s.pending, queuedDelivery{delivery: Delivery{Record: &copy}, bytes: recordBytes})
		s.pendingBytes += recordBytes
	}
	s.mu.Unlock()
	s.wake()
}

func (s *subscriber) enqueueGap(reason GapReason, oldest, current Cursor) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.replaceWithGapLocked(reason, oldest, current)
	s.mu.Unlock()
	s.wake()
}

func (s *subscriber) replaceWithGapLocked(reason GapReason, oldest, current Cursor) {
	gap := Gap{Reason: reason, Requested: s.delivered, OldestAvailable: oldest, Current: current}
	for index := range s.pending {
		s.pending[index] = queuedDelivery{}
	}
	s.pending = s.pending[:0]
	s.pendingBytes = 0
	data, _ := json.Marshal(Delivery{Gap: &gap})
	s.pending = append(s.pending, queuedDelivery{delivery: Delivery{Gap: &gap}, bytes: len(data)})
	s.pendingBytes = len(data)
}

func (s *subscriber) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) run() {
	defer close(s.output)
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}
		for {
			s.mu.Lock()
			if len(s.pending) == 0 {
				s.mu.Unlock()
				break
			}
			queued := s.pending[0]
			s.pending[0] = queuedDelivery{}
			s.pending = s.pending[1:]
			s.pendingBytes -= queued.bytes
			s.mu.Unlock()
			value := queued.delivery
			select {
			case s.output <- value:
				s.mu.Lock()
				if value.Record != nil {
					s.delivered = value.Record.Cursor
				} else if value.Gap != nil {
					s.delivered = value.Gap.Current
				}
				s.mu.Unlock()
			case <-s.done:
				return
			}
		}
	}
}

func (s *subscriber) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for index := range s.pending {
			s.pending[index] = queuedDelivery{}
		}
		s.pending = nil
		s.pendingBytes = 0
		s.mu.Unlock()
		close(s.done)
	})
}
