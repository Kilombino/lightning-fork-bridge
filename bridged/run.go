package bridged

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Run starts the daemon and blocks until ctx ends or the listener fails.
//
// The order matters. Pending swaps are resumed before the surface opens, so a
// swap interrupted by the last shutdown is being driven again before any new
// one can be quoted: an HTLC that is already locked in has a deadline, and a
// new quote can wait.
func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctxOrBackground(ctx))
	defer cancel()
	s.ctx = ctx

	// Observers first, and a moment to fill: a resumed swap is sized
	// against block spacing, and an observer with nothing in it refuses.
	go s.pollChains(ctx)
	go s.pollPrice(ctx)

	if err := s.waitForChains(ctx); err != nil {
		return err
	}

	if err := s.resume(ctx); err != nil {
		// Not fatal. A swap that cannot be resumed is a swap that needs an
		// operator, and refusing to start would take the others down with
		// it and close the surface that reports them.
		s.log.Error("some swaps could not be resumed", "err", err)
	}

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.log.Info("bridge is up", "listen", ln.Addr().String(),
		"directions", len(s.sides))

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case <-ctx.Done():
	}

	// Stop accepting, then let the swaps in flight finish. A swap holds an
	// HTLC on someone's channel; dropping it to exit a second sooner is how
	// one expires.
	shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	_ = srv.Shutdown(shutdown)

	s.log.Info("waiting for swaps in flight", "swaps", s.active())
	s.wg.Wait()

	return nil
}

// active is how many swaps are being driven, across directions.
func (s *Service) active() int {
	var n int
	for _, sd := range s.sides {
		n += sd.runner.Active()
	}

	return n
}

// waitForChains blocks until both observers can estimate, or ctx ends.
//
// Without this the daemon would open its surface and refuse every quote until
// the observers filled, which looks like a broken bridge rather than a
// starting one.
func (s *Service) waitForChains(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	var last error
	for {
		_, lfErr := s.lfChain.Estimate(time.Now())
		_, btcErr := s.btcChain.Estimate(time.Now())
		if lfErr == nil && btcErr == nil {
			return nil
		}
		last = errors.Join(lfErr, btcErr)

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for both chains to be "+
				"measurable: %w", last)
		case <-tick.C:
		}
	}
}

// resume drives every swap the journal says is unfinished.
//
// Each is routed to the direction whose paying node can read its outgoing
// invoice, because that is the node that will pay it. Running every direction's
// Resume over a shared journal would have both of them drive every swap, and
// the one whose nodes are the wrong way round would decide against a chain the
// swap is not on.
func (s *Service) resume(ctx context.Context) error {
	pending, err := s.journal.Pending(ctx)
	if err != nil {
		return fmt.Errorf("reading pending swaps: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}

	s.log.Info("resuming", "swaps", len(pending))

	var errs []error
	for _, rec := range pending {
		sd, _, err := s.route(ctx, rec.Invoice)
		if err != nil {
			errs = append(errs, fmt.Errorf("%x: %w", rec.Hash, err))

			continue
		}
		s.drive(sd, rec.Hash)
	}

	return errors.Join(errs...)
}
