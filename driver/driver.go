// Package driver turns node facts into swap events and carries out what the
// state machine decides.
//
// Two of its jobs are the ones that lose money when they are wrong.
//
// Gate runs immediately before the outgoing payment and is the last moment a
// swap can be abandoned for free. It reads both chains fresh rather than
// trusting what the quote was built from, because a quote minutes old was
// sized against a block rate that may since have changed.
//
// Resume runs after a restart and decides what a swap left mid-flight should
// do. Its hardest case is a swap recorded as Paying whose outgoing node has no
// record of the payment: that is refused rather than guessed at, because the
// two readings of it, "never sent" and "sent and lost track of", call for
// opposite actions and only one of them is recoverable.
package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

var (
	// ErrNeedsOperator is returned when the bridge cannot safely decide on
	// its own. It never accompanies an action; the swap is left where it is
	// for a human to look at.
	ErrNeedsOperator = errors.New("swap needs an operator")

	// ErrState is returned when a swap is asked to do something its state
	// does not allow.
	ErrState = errors.New("swap is not in a state for this")

	// ErrStaleRates is returned when the block-spacing measurement handed to
	// Gate is too old to decide on.
	ErrStaleRates = errors.New("block spacing measurement is stale")
)

// Swap is the record the driver works from. It holds only what the decisions
// need; amounts, rates and quote details live with the caller.
type Swap struct {
	// Hash appears on both chains.
	Hash node.Hash

	// State is where the machine has got to.
	State swap.State

	// Preimage is known once the outgoing leg succeeds.
	Preimage node.Preimage

	// OutgoingCLTVLimit is the CLTV budget the bridge imposes on the
	// outgoing payment, in blocks of the outgoing chain. The incoming leg
	// has to outlive it.
	OutgoingCLTVLimit uint32
}

// Rates are the block-spacing bounds for both chains, with the moment they
// were measured.
//
// The moment matters: bounds are derived from a chain's recent behaviour, and
// behaviour an hour old is not evidence about now. Gate refuses rather than
// trusting a stale measurement, because a swap sized against a block rate the
// chain has since left is exactly the failure the margin policy exists to
// prevent.
type Rates struct {
	Incoming margin.Bounds
	Outgoing margin.Bounds

	// As is when these were measured. A zero value is refused.
	As time.Time
}

// DefaultMaxRateAge is how old a measurement may be before Gate refuses to
// decide on it. Blocks on the BLAKE2b chain arrive every few minutes, so a
// measurement this old still describes the present; much older and it does
// not.
const DefaultMaxRateAge = 5 * time.Minute

// Driver holds the two nodes and the safety policy.
type Driver struct {
	In     node.Incoming
	Out    node.Outgoing
	Policy margin.Policy

	// MaxRateAge overrides DefaultMaxRateAge. Zero means the default.
	MaxRateAge time.Duration

	// Now is the clock, for tests. Zero means time.Now.
	Now func() time.Time
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}

	return time.Now()
}

func (d *Driver) maxRateAge() time.Duration {
	if d.MaxRateAge > 0 {
		return d.MaxRateAge
	}

	return DefaultMaxRateAge
}

// Gate decides whether the outgoing payment may be committed.
//
// It returns swap.MarginPassed or swap.MarginRefused, which the caller feeds
// to swap.Step. A refusal is not an error: it is the abort-rather-than-pay
// decision working. An error means the decision could not be made at all,
// which must also not result in a payment.
func (d *Driver) Gate(ctx context.Context, sw *Swap, r Rates) (swap.Event, error) {
	if sw.State != swap.Funded {
		return swap.MarginRefused, fmt.Errorf("%w: gate needs %v, have %v",
			ErrState, swap.Funded, sw.State)
	}

	if r.As.IsZero() {
		return swap.MarginRefused, fmt.Errorf("%w: rates carry no "+
			"measurement time", ErrStaleRates)
	}
	if age := d.now().Sub(r.As); age > d.maxRateAge() {
		return swap.MarginRefused, fmt.Errorf("%w: measured %v ago, limit %v",
			ErrStaleRates, age, d.maxRateAge())
	}

	inv, err := d.In.LookupInvoice(ctx, sw.Hash)
	if err != nil {
		return swap.MarginRefused, fmt.Errorf("reading the incoming "+
			"invoice: %w", err)
	}
	// The gate exists to protect a claim that is actually there. If the
	// HTLC is not locked in, there is nothing to pay out against.
	if !inv.State.Claimable() {
		return swap.MarginRefused, nil
	}

	height, err := d.In.BlockHeight(ctx)
	if err != nil {
		return swap.MarginRefused, fmt.Errorf("reading the incoming "+
			"height: %w", err)
	}
	if inv.ExpiryHeight <= height {
		// Already expired, or expiring in this block.
		return swap.MarginRefused, nil
	}
	remaining := inv.ExpiryHeight - height

	err = margin.Check(
		margin.Leg{Bounds: r.Incoming, Blocks: uint32(remaining)},
		margin.Leg{Bounds: r.Outgoing, Blocks: sw.OutgoingCLTVLimit},
		d.Policy,
	)
	switch {
	case err == nil:
		return swap.MarginPassed, nil

	case errors.Is(err, margin.ErrUnsafe):
		// The margin is not there. This is the refusal the whole policy
		// exists to produce, and it is an ordinary outcome, not a fault.
		return swap.MarginRefused, nil

	default:
		// Bad bounds or bad inputs. Refuse, and say why, because an
		// unusable measurement is a reason to stop quoting.
		return swap.MarginRefused, fmt.Errorf("margin check: %w", err)
	}
}

// Resume decides what a swap found mid-flight after a restart should do.
//
// It returns an event to apply, or ErrNeedsOperator when the bridge must not
// decide on its own.
func (d *Driver) Resume(ctx context.Context, sw *Swap) (swap.Event, bool, error) {
	if sw.State.Terminal() {
		return 0, false, nil
	}

	if sw.State == swap.Paying {
		return d.resumePaying(ctx, sw)
	}

	// Everything else is decided by what the incoming node says.
	inv, err := d.In.LookupInvoice(ctx, sw.Hash)
	if err != nil {
		if errors.Is(err, node.ErrUnknownHash) && sw.State == swap.Quoted {
			// Nothing was ever created, which is consistent.
			return 0, false, nil
		}

		return 0, false, fmt.Errorf("reading the incoming invoice: %w", err)
	}

	switch sw.State {
	case swap.Offered:
		switch inv.State {
		case node.InvoiceAccepted:
			return swap.IncomingAccepted, true, nil
		case node.InvoiceCancelled:
			return swap.IncomingLapsed, true, nil
		}

	case swap.Paid:
		// The settle may already have gone through before the crash.
		if inv.State == node.InvoiceSettled {
			return swap.IncomingSettled, true, nil
		}
		if !inv.State.Claimable() {
			// Paid out and the claim is gone: a realised loss, and the
			// machine has a name for it.
			return swap.IncomingUnsettleable, true, nil
		}

	case swap.Failing, swap.Aborting, swap.Expiring:
		if inv.State == node.InvoiceCancelled {
			return swap.IncomingCancelled, true, nil
		}
	}

	// Nothing has changed since the crash; the outstanding action is
	// reissued by the caller using swap.Recoverable.
	return 0, false, nil
}

// resumePaying is the delicate one. A swap was recorded as Paying, so an
// outgoing payment may or may not have left.
func (d *Driver) resumePaying(ctx context.Context, sw *Swap) (swap.Event, bool,
	error) {

	pmt, err := d.Out.LookupPayment(ctx, sw.Hash)
	if err != nil {
		return 0, false, fmt.Errorf("reading the outgoing payment: %w", err)
	}

	switch pmt.State {
	case node.PaymentSucceeded:
		sw.Preimage = pmt.Preimage

		return swap.OutgoingSucceeded, true, nil

	case node.PaymentFailed:
		return swap.OutgoingFailed, true, nil

	case node.PaymentInFlight:
		// Still going. Wait for it; concluding anything here is exactly
		// the mistake the state machine refuses to allow.
		return 0, false, nil

	case node.PaymentNone:
		// The swap says a payment was started and the node has no record
		// of it. Either it never left, in which case cancelling the
		// incoming HTLC is correct, or the node lost track of one that
		// did, in which case cancelling gives away the only claim
		// against money that has already gone. The two readings call for
		// opposite actions and the bridge cannot tell them apart, so it
		// stops and asks.
		return 0, false, fmt.Errorf("%w: recorded as %v but the outgoing "+
			"node has no record of the payment; do not cancel the "+
			"incoming HTLC until the outgoing leg is accounted for",
			ErrNeedsOperator, swap.Paying)
	}

	return 0, false, fmt.Errorf("%w: unknown outgoing payment state %v",
		ErrNeedsOperator, pmt.State)
}

// Do carries out an action. Every action is idempotent on its node, so a
// repeat after a crash is safe.
func (d *Driver) Do(ctx context.Context, sw *Swap, a swap.Action) error {
	switch a {
	case swap.Nothing:
		return nil

	case swap.CancelIncoming:
		if sw.State.Committed() {
			// Belt and braces behind the state machine: nothing may
			// cancel while a payment might still be in flight.
			return fmt.Errorf("%w: refusing to cancel while %v",
				ErrState, sw.State)
		}

		return d.In.CancelInvoice(ctx, sw.Hash)

	case swap.SettleIncoming:
		var zero node.Preimage
		if sw.Preimage == zero {
			return fmt.Errorf("%w: no preimage to settle with", ErrState)
		}

		return d.In.SettleInvoice(ctx, sw.Preimage)

	case swap.PayOutgoing:
		return fmt.Errorf("%w: paying is not a fire-and-forget action; "+
			"use Pay so the outcome is recorded", ErrState)
	}

	return fmt.Errorf("%w: unknown action %v", ErrState, a)
}

// Pay sends the outgoing payment and reports the event its outcome implies.
//
// A timeout or transport error yields no event: the payment may still be in
// flight, and only LookupPayment can say. The swap stays in Paying, which is
// precisely the state that forbids cancelling the incoming HTLC.
func (d *Driver) Pay(ctx context.Context, sw *Swap, req node.PayRequest) (
	swap.Event, bool, error) {

	if sw.State != swap.Paying {
		return 0, false, fmt.Errorf("%w: pay needs %v, have %v", ErrState,
			swap.Paying, sw.State)
	}

	pmt, err := d.Out.Pay(ctx, req)
	if err != nil {
		return 0, false, fmt.Errorf("outgoing payment: %w", err)
	}

	switch pmt.State {
	case node.PaymentSucceeded:
		sw.Preimage = pmt.Preimage

		return swap.OutgoingSucceeded, true, nil

	case node.PaymentFailed:
		return swap.OutgoingFailed, true, nil

	default:
		// In flight or unknown. Nothing to conclude.
		return 0, false, nil
	}
}
