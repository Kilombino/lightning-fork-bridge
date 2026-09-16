package bridged

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Run must not return while a swap is still being driven, whatever ended it.
// The caller's next move is Close, which shuts the node connections those
// swaps are talking over, and resume starts driving before the listener is
// opened: a failure to listen used to pull the connections out from under
// them.
func TestRunWaitsForSwapsEvenWhenListeningFails(t *testing.T) {
	t.Parallel()

	// Hold the port so Listen fails, which is the path that used to skip
	// the wait.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	s := quotableService(t)
	s.cfg.Listen = busy.Addr().String()
	s.cfg.PollInterval = time.Hour
	s.cfg.CompactInterval = time.Hour

	// A swap that is still running when Run gives up.
	var finished atomic.Bool
	release := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-release
		finished.Store(true)
	}()

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	// Run should be blocked on the swap, not returned.
	select {
	case err := <-done:
		t.Fatalf("Run returned while a swap was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listening") {
			t.Fatalf("wanted the listen failure reported, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the swap finished")
	}
	if !finished.Load() {
		t.Error("Run returned before the swap was done")
	}
}

// Shutting down has to stop accepting before the caller stops driving, and
// still return once the swaps are done.
func TestRunShutsDownCleanly(t *testing.T) {
	t.Parallel()

	s := quotableService(t)
	s.cfg.Listen = "127.0.0.1:0"
	s.cfg.PollInterval = time.Hour
	s.cfg.CompactInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give it a moment to get past waitForChains and open the listener.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled run should end cleanly: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A daemon whose chains never become measurable must give up when told to,
// rather than waiting forever with no surface to ask.
func TestWaitForChainsGivesUpWithTheContext(t *testing.T) {
	t.Parallel()

	s := quotableService(t)
	// Observers with nothing in them, so no estimate is possible.
	s.lfChain = emptyObserver(t)
	s.btcChain = emptyObserver(t)

	ctx, cancel := context.WithTimeout(context.Background(),
		200*time.Millisecond)
	defer cancel()

	err := s.waitForChains(ctx)
	if err == nil {
		t.Fatal("wanted waitForChains to give up")
	}
	if !strings.Contains(err.Error(), "measurable") {
		t.Errorf("the error should say what it was waiting for: %v", err)
	}
}
