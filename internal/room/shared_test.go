package room

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/store"
)

func TestSharedCapacityQueuesCancelsAndReleasesAcrossRooms(t *testing.T) {
	s := NewSharedCapacity()
	wait := func() {}
	release, err := s.acquireProvider(t.Context(), "one", chat.Codex, 1, wait)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{})
	granted := make(chan func(), 1)
	go func() {
		done, err := s.acquireProvider(t.Context(), "two", chat.Codex, 1, func() { close(queued) })
		if err == nil {
			granted <- done
		}
	}()
	<-queued
	select {
	case <-granted:
		t.Fatal("provider capacity exceeded")
	default:
	}
	release()
	select {
	case done := <-granted:
		done()
	case <-time.After(time.Second):
		t.Fatal("queued room did not resume")
	}
	release, _ = s.acquireProvider(t.Context(), "one", chat.Codex, 1, wait)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.acquireProvider(ctx, "cancelled", chat.Codex, 1, wait); err == nil {
		t.Fatal("cancelled call accepted")
	}
	release()
}

func TestIndependentRoomsSerializeWritersAndKeepReadOnlyWorkMoving(t *testing.T) {
	s, _ := store.New(t.TempDir())
	workspace := t.TempDir()
	shared := NewSharedCapacity()
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	readStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	makeRoom := func(run func(chat.Participant, context.Context, func(agent.Event)) (agent.TurnResult, error)) *Orchestrator {
		state, err := s.Create(workspace, 1)
		if err != nil {
			t.Fatal(err)
		}
		agents := []agent.Agent{}
		for _, p := range []chat.Participant{chat.Codex, chat.Claude} {
			agents = append(agents, &fakeAgent{participant: p, run: func(ctx context.Context, _ int, _ agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
				return run(p, ctx, emit)
			}})
		}
		o, err := New(state, nil, s, agents...)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.ConfigureSharedCapacity(shared); err != nil {
			t.Fatal(err)
		}
		go func() {
			for range o.Events() {
			}
		}()
		t.Cleanup(func() { _ = o.Close() })
		return o
	}
	one := makeRoom(func(p chat.Participant, ctx context.Context, emit func(agent.Event)) (agent.TurnResult, error) {
		for range 700 {
			emit(agent.Event{Type: agent.EventStatus, Agent: p, Text: "working"})
		}
		firstStarted <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return agent.TurnResult{}, ctx.Err()
		}
		return agent.TurnResult{Text: "first room result", Done: true}, nil
	})
	two := makeRoom(func(_ chat.Participant, _ context.Context, _ func(agent.Event)) (agent.TurnResult, error) {
		secondStarted <- struct{}{}
		return agent.TurnResult{Text: "second room result", Done: true}, nil
	})
	three := makeRoom(func(_ chat.Participant, _ context.Context, _ func(agent.Event)) (agent.TurnResult, error) {
		readStarted <- struct{}{}
		return agent.TurnResult{Text: "read-only result", Done: true}, nil
	})
	if err := one.Post("@codex change the parser"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("background events stalled the first room")
	}
	if err := two.Post("@claude change persistence"); err != nil {
		t.Fatal(err)
	}
	if err := three.Ask("@claude inspect external evidence"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting writer blocked unrelated read-only work")
	}
	select {
	case <-secondStarted:
		t.Fatal("two rooms wrote the same checkout concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second room did not resume after writer completed")
	}
	one.wg.Wait()
	two.wg.Wait()
	three.wg.Wait()
}

func TestSharedCapacityLimitIncreaseWakesQueuedRoom(t *testing.T) {
	s := NewSharedCapacity()
	first, err := s.acquireProvider(t.Context(), "one", chat.Codex, 1, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	queued, granted := make(chan struct{}), make(chan func(), 1)
	go func() {
		release, err := s.acquireProvider(t.Context(), "two", chat.Codex, 1, func() { close(queued) })
		if err == nil {
			granted <- release
		}
	}()
	<-queued
	s.setProviderLimit(chat.Codex, 2)
	select {
	case release := <-granted:
		release()
	case <-time.After(time.Second):
		t.Fatal("capacity increase did not wake queued room")
	}
}

func TestSharedWriterRemainsHeldUntilCancelledNativeCallReturns(t *testing.T) {
	s := NewSharedCapacity()
	if err := s.acquireWriter(t.Context(), "checkout", "one", func() {}); err != nil {
		t.Fatal(err)
	}
	s.retainWriterTurn("one")
	s.releaseWriter("checkout", "one")
	queued, granted := make(chan struct{}), make(chan error, 1)
	go func() { granted <- s.acquireWriter(t.Context(), "checkout", "two", func() { close(queued) }) }()
	<-queued
	select {
	case <-granted:
		t.Fatal("writer started while cancelled native call was still running")
	default:
	}
	s.finishWriterTurn("checkout", "one")
	select {
	case err := <-granted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not resume after native call returned")
	}
	s.releaseWriter("checkout", "two")
}

func TestSharedCheckoutResolvesSymlinkAliases(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(o.room.Workspace, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical := o.room.Workspace
	o.room.Workspace = alias
	if err := o.ConfigureSharedCapacity(NewSharedCapacity()); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if o.sharedWorkspace != want {
		t.Fatalf("checkout alias = %q, want %q", o.sharedWorkspace, want)
	}
}

func TestBackgroundApprovalSurvivesViewSwitchUntilAnswered(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	defer o.Close()
	go func() {
		for range o.Events() {
		}
	}()
	request := &agent.ApprovalRequest{Agent: chat.Codex, Title: "Approve this operation", Response: make(chan agent.ApprovalDecision, 1)}
	event := agent.Event{Type: agent.EventApproval, Agent: chat.Codex, Approval: request}
	o.send(Event{Type: EventAgent, AgentEvent: &event})
	view, stop := o.SubscribeView()
	select {
	case got := <-view:
		if got.AgentEvent.Approval != request {
			t.Fatal("wrong approval")
		}
	case <-time.After(time.Second):
		t.Fatal("pending approval lost")
	}
	stop()
	o.ForgetApproval(request)
	view, stop = o.SubscribeView()
	defer stop()
	select {
	case <-view:
		t.Fatal("answered approval replayed")
	default:
	}
}

func TestSharedWriterLeaseCoversWorkflowAndSeparatesCheckouts(t *testing.T) {
	s := NewSharedCapacity()
	wait := func() {}
	if err := s.acquireWriter(t.Context(), "checkout", "one:workflow", wait); err != nil {
		t.Fatal(err)
	}
	if err := s.acquireWriter(t.Context(), "checkout", "one:workflow", wait); err != nil {
		t.Fatal("same workflow cannot reenter", err)
	}
	if err := s.acquireWriter(t.Context(), "other-checkout", "two:other", wait); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- s.acquireWriter(ctx, "checkout", "two:workflow", func() { close(waiting) }) }()
	<-waiting
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled writer acquired lease")
	}
	var acquired atomic.Bool
	waiting = make(chan struct{})
	done = make(chan error, 1)
	go func() {
		err := s.acquireWriter(t.Context(), "checkout", "three:workflow", func() { close(waiting) })
		acquired.Store(err == nil)
		done <- err
	}()
	<-waiting
	if acquired.Load() {
		t.Fatal("overlapping writers")
	}
	s.releaseWriter("checkout", "one:workflow")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not resume")
	}
	s.releaseWriter("checkout", "three:workflow")
	s.releaseWriter("other-checkout", "two:other")
}
