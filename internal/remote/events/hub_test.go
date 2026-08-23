package events

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
)

func TestReplayUsesStableCursorsAndTracksMessageHighWater(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "boot-a", MaxRecords: 8, MaxBytes: 1 << 20})
	defer hub.Close()

	first, err := hub.Publish(testEvent("first", 0, "one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Publish(testEvent("second", 17, "two"))
	if err != nil {
		t.Fatal(err)
	}
	if first != (Cursor{BootID: "boot-a", EventSequence: 1}) {
		t.Fatalf("first cursor=%+v", first)
	}
	if second != (Cursor{BootID: "boot-a", EventSequence: 2, MessageSequence: 17}) {
		t.Fatalf("second cursor=%+v", second)
	}

	subscription, err := hub.Subscribe(first, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Gap != nil || subscription.Current != second || len(subscription.Replay) != 1 {
		t.Fatalf("subscription=%+v", subscription)
	}
	if got := subscription.Replay[0]; got.Cursor != second || got.Event.ID != "second" || got.Event.Payload.Message.Sequence != 17 {
		t.Fatalf("replay=%+v", got)
	}

	third, err := hub.Publish(testEvent("third", 12, "three"))
	if err != nil {
		t.Fatal(err)
	}
	if third.MessageSequence != 17 {
		t.Fatalf("message high-water regressed: %+v", third)
	}
	delivery := receive(t, subscription.Events)
	if delivery.Record == nil || delivery.Record.Cursor != third || delivery.Record.Event.ID != "third" {
		t.Fatalf("live delivery=%+v", delivery)
	}
}

func TestInitialMessageSequenceSeedsCurrentWithoutCreatingEvent(t *testing.T) {
	hub := newTestHub(t, Options{
		BootID: "boot", InitialMessageSequence: 41, MaxRecords: 4, MaxBytes: 1 << 20,
	})
	defer hub.Close()
	if current := hub.Current(); current != (Cursor{BootID: "boot", MessageSequence: 41}) {
		t.Fatalf("initial cursor=%+v", current)
	}
	subscription, err := hub.Subscribe(Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Current != (Cursor{BootID: "boot", MessageSequence: 41}) || len(subscription.Replay) != 0 || subscription.Gap != nil {
		t.Fatalf("initial subscription=%+v", subscription)
	}

	cursor, err := hub.Publish(testEvent("older-message", 17, "older"))
	if err != nil {
		t.Fatal(err)
	}
	if cursor != (Cursor{BootID: "boot", EventSequence: 1, MessageSequence: 41}) {
		t.Fatalf("published cursor=%+v", cursor)
	}
}

func TestReplayConsumersReceiveIndependentSanitizedValues(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 4, MaxBytes: 1 << 20})
	defer hub.Close()
	if _, err := hub.Publish(testEvent("message", 8, "original")); err != nil {
		t.Fatal(err)
	}
	after := Cursor{BootID: "boot"}
	first, err := hub.Subscribe(after, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Replay) != 1 {
		t.Fatalf("first replay=%d", len(first.Replay))
	}
	first.Replay[0].Event.Payload.Message.Text = "mutated"
	first.Cancel()

	second, err := hub.Subscribe(after, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cancel()
	if got := second.Replay[0].Event.Payload.Message.Text; got != "original" {
		t.Fatalf("retained sanitized event was mutated: %q", got)
	}
}

func TestSubscribeReportsBootMismatch(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "new-boot", MaxRecords: 4, MaxBytes: 1 << 20})
	defer hub.Close()
	current, err := hub.Publish(testEvent("current", 4, "message"))
	if err != nil {
		t.Fatal(err)
	}
	requested := Cursor{BootID: "old-boot", EventSequence: 99, MessageSequence: 3}
	subscription, err := hub.Subscribe(requested, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Gap == nil || subscription.Gap.Reason != GapBootMismatch || subscription.Gap.Requested != requested || subscription.Gap.Current != current {
		t.Fatalf("gap=%+v", subscription.Gap)
	}
	if len(subscription.Replay) != 0 {
		t.Fatalf("boot mismatch replayed %d records", len(subscription.Replay))
	}
}

func TestSubscribeReportsExpiredCursorAfterRecordEviction(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 2, MaxBytes: 1 << 20})
	defer hub.Close()
	for index := 1; index <= 3; index++ {
		if _, err := hub.Publish(testEvent(fmt.Sprintf("event-%d", index), 0, "value")); err != nil {
			t.Fatal(err)
		}
	}
	subscription, err := hub.Subscribe(Cursor{BootID: "boot", EventSequence: 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Gap == nil || subscription.Gap.Reason != GapCursorExpired {
		t.Fatalf("gap=%+v", subscription.Gap)
	}
	if subscription.Gap.OldestAvailable.EventSequence != 2 || subscription.Gap.Current.EventSequence != 3 {
		t.Fatalf("gap bounds=%+v", subscription.Gap)
	}
}

func TestSubscriberOverflowProducesGapWithoutAnotherPublish(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 16, MaxBytes: 1 << 20})
	defer hub.Close()
	subscription, err := hub.Subscribe(Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	for index := 1; index <= 4; index++ {
		if _, err := hub.Publish(testEvent(fmt.Sprintf("event-%d", index), 0, "value")); err != nil {
			t.Fatal(err)
		}
	}
	// No fifth publish is used to flush a warning. Once the consumer resumes, the
	// subscriber's already-enqueued structured gap must arrive on its own.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case delivery, ok := <-subscription.Events:
			if !ok {
				t.Fatal("subscription closed before overflow gap")
			}
			if delivery.Gap != nil {
				if delivery.Gap.Reason != GapSubscriberOverflow || delivery.Gap.Current.EventSequence < 2 || delivery.Gap.Current.EventSequence > 4 {
					t.Fatalf("gap=%+v", delivery.Gap)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for overflow gap")
		}
	}
}

func TestSubscriberQueueAlsoHonorsEncodedByteCap(t *testing.T) {
	probe := newTestHub(t, Options{BootID: "probe", MaxRecords: 4, MaxBytes: 1 << 20})
	if _, err := probe.Publish(testEvent("probe", 0, strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	oneRecordBytes := probe.recordBytes
	probe.mu.Unlock()
	probe.Close()

	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 16, MaxBytes: oneRecordBytes + 32})
	defer hub.Close()
	subscription, err := hub.Subscribe(Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	for index := 1; index <= 4; index++ {
		if _, err := hub.Publish(testEvent(fmt.Sprintf("large-%d", index), 0, strings.Repeat("x", 400))); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case delivery := <-subscription.Events:
			if delivery.Gap != nil {
				if delivery.Gap.Reason != GapSubscriberOverflow {
					t.Fatalf("gap=%+v", delivery.Gap)
				}
				return
			}
		case <-deadline:
			t.Fatal("byte-bounded subscriber did not report a gap")
		}
	}
}

func TestConcurrentPublishHasOneStrictSequence(t *testing.T) {
	const count = 256
	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: count, MaxBytes: 8 << 20})
	defer hub.Close()

	var wait sync.WaitGroup
	errors := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := hub.Publish(testEvent(fmt.Sprintf("event-%d", index), uint64(index+1), "value"))
			errors <- err
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	subscription, err := hub.Subscribe(Cursor{BootID: "boot"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Gap != nil || len(subscription.Replay) != count {
		t.Fatalf("gap=%+v replay=%d", subscription.Gap, len(subscription.Replay))
	}
	for index, record := range subscription.Replay {
		if want := uint64(index + 1); record.Cursor.EventSequence != want {
			t.Fatalf("replay[%d] sequence=%d want=%d", index, record.Cursor.EventSequence, want)
		}
	}
	if subscription.Current.MessageSequence != count {
		t.Fatalf("message high-water=%d", subscription.Current.MessageSequence)
	}
}

func TestReplayRingHonorsApproximateJSONByteCap(t *testing.T) {
	probe := newTestHub(t, Options{BootID: "boot", MaxRecords: 10, MaxBytes: 1 << 20})
	if _, err := probe.Publish(testEvent("probe", 0, strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	oneRecordBytes := probe.recordBytes
	probe.mu.Unlock()
	probe.Close()

	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 10, MaxBytes: oneRecordBytes + 32})
	defer hub.Close()
	if _, err := hub.Publish(testEvent("first", 0, strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Publish(testEvent("second", 0, strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	records, retainedBytes := len(hub.records), hub.recordBytes
	hub.mu.Unlock()
	if records != 1 || retainedBytes > oneRecordBytes+32 {
		t.Fatalf("records=%d bytes=%d cap=%d", records, retainedBytes, oneRecordBytes+32)
	}
	subscription, err := hub.Subscribe(Cursor{BootID: "boot"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if subscription.Gap == nil || subscription.Gap.Reason != GapCursorExpired || subscription.Gap.OldestAvailable.EventSequence != 2 {
		t.Fatalf("byte-cap gap=%+v", subscription.Gap)
	}
}

func TestCancelAndCloseEndStreamsAndRejectNewWork(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "boot", MaxRecords: 2, MaxBytes: 1024})
	first, err := hub.Subscribe(Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	first.Cancel()
	assertClosed(t, first.Events)

	second, err := hub.Subscribe(Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	hub.Close()
	assertClosed(t, second.Events)
	if _, err := hub.Publish(testEvent("late", 0, "late")); !errorsIs(err, ErrClosed) {
		t.Fatalf("publish after close error=%v", err)
	}
	if _, err := hub.Subscribe(Cursor{}, 1); !errorsIs(err, ErrClosed) {
		t.Fatalf("subscribe after close error=%v", err)
	}
}

func TestInvalidateRotatesBootAndBroadcastsReservedGap(t *testing.T) {
	hub := newTestHub(t, Options{BootID: "old-boot", InitialMessageSequence: 9, MaxRecords: 4, MaxBytes: 4096})
	defer hub.Close()
	subscription, err := hub.Subscribe(Cursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if _, err := hub.Publish(testEvent("before", 10, "before")); err != nil {
		t.Fatal(err)
	}
	if delivery := receive(t, subscription.Events); delivery.Record == nil {
		t.Fatalf("initial delivery=%+v", delivery)
	}
	current, err := hub.Invalidate(GapUpstreamOverflow)
	if err != nil {
		t.Fatal(err)
	}
	if current.BootID == "old-boot" || current.EventSequence != 0 || current.MessageSequence != 10 {
		t.Fatalf("current=%+v", current)
	}
	delivery := receive(t, subscription.Events)
	if delivery.Gap == nil || delivery.Gap.Reason != GapUpstreamOverflow || delivery.Gap.Current != current {
		t.Fatalf("delivery=%+v", delivery)
	}
	reconnected, err := hub.Subscribe(Cursor{BootID: "old-boot", EventSequence: 1, MessageSequence: 10}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Cancel()
	if reconnected.Gap == nil || reconnected.Gap.Reason != GapBootMismatch {
		t.Fatalf("reconnected gap=%+v", reconnected.Gap)
	}
}

func newTestHub(t *testing.T, options Options) *Hub {
	t.Helper()
	hub, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

func testEvent(id string, sequence uint64, text string) api.Event {
	event := api.Event{Version: api.Version, ID: id, Type: "event", RoomID: "room", Payload: api.EventPayload{Text: text}}
	if sequence != 0 {
		event.Payload.Message = &api.MessageView{ID: "message-" + id, Sequence: sequence, Text: text}
	}
	return event
}

func receive(t *testing.T, stream <-chan Delivery) Delivery {
	t.Helper()
	select {
	case value, ok := <-stream:
		if !ok {
			t.Fatal("stream closed")
		}
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
		return Delivery{}
	}
}

func assertClosed(t *testing.T, stream <-chan Delivery) {
	t.Helper()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("stream remained open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream close")
	}
}

func errorsIs(err, target error) bool {
	return err != nil && errors.Is(err, target)
}
