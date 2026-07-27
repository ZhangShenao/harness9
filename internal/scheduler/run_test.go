package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestRecoverInterruptedMarksRunningAttemptsIndeterminate(t *testing.T) {
	store := newTestStore(t)
	approvedMissionWithTwoRootTasks(t, store, `{"max_concurrent_tasks":2}`)
	dispatcher := &fakeDispatcher{store: store, failTask: map[string]bool{}}
	s := NewScheduler(store, dispatcher, WithMaxGlobalConcurrency(10))
	dispatched, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if dispatched != 2 {
		t.Fatalf("dispatched = %d, want 2 running attempts before recovery", dispatched)
	}

	count, err := s.RecoverInterrupted(context.Background())
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if count != 2 {
		t.Fatalf("RecoverInterrupted count = %d, want 2", count)
	}
}

func TestRunStopsWhenContextIsCancelled(t *testing.T) {
	store := newTestStore(t)
	dispatcher := &fakeDispatcher{store: store, failTask: map[string]bool{}}
	s := NewScheduler(store, dispatcher, WithMaxGlobalConcurrency(10))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 5*time.Millisecond) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop within 1s of context cancellation")
	}
}
