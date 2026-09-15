// Package quote turns "pay this invoice" into a swap, or into a refusal.
//
// It is where a swap acquires its terms, and therefore where most of the ways
// a bridge can lose money quietly are ruled out: an invoice that expires
// before the incoming leg could be funded, a routing budget wider than the
// spread it is paid out of, an incoming amount rounded in the payer's favour
// on every swap, a second hold invoice created for a hash that already has
// one.
//
// It never chooses the payment hash. The hash comes from the invoice the user
// asked to have paid, read by the node that will pay it, and the incoming hold
// invoice is built on that exact value. That is the whole mechanism: one
// preimage claims both legs.
package quote

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/paulscode/lightning-fork-bridge/driver"
	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

var (
	// ErrRefused is returned when the bridge will not quote this swap. It is
	// an ordinary outcome, not a fault.
	ErrRefused = errors.New("bridge will not quote this swap")

	// ErrDuplicate is returned when a swap already exists for a payment hash.
	ErrDuplicate = errors.New("a swap already exists for this payment hash")

	// ErrPolicy is returned for a quoting policy that cannot be used.
	ErrPolicy = errors.New("quote policy is not usable")
)

// Policy is the terms the bridge is willing to offer.
type Policy struct {
	// MinSwapMsat and MaxSwapMsat bound the outgoing leg, in millisatoshis of
	// the outgoing chain. That is the side the bridge's own money leaves
	// from, so it is the side a cap has to be expressed in: the maximum comes
	// from the capital model, where the per-swap cap is the inventory band
	// over ten, and no single swap may take out more than a tenth of what is
	// at risk.
	MinSwapMsat uint64
	MaxSwapMsat uint64

	// Validity is how long a quoted rate is honoured.
	Validity time.Duration

	// InvoiceExpiry is how long the hold invoice lives. It must be at least
	// Validity, or a quote could outlive the invoice it is for.
	InvoiceExpiry time.Duration

	// OutgoingCLTVLimit is the CLTV budget imposed on the outgoing route, in
	// blocks of the outgoing chain. Everything the incoming leg must outlive
	// follows from this, so smaller is cheaper.
	OutgoingCLTVLimit uint32

	// FeeFraction is the routing budget for the outgoing leg, as a fraction
	// of its amount.
	FeeFraction float64

	// MinRemainingLife is how long the outgoing invoice must still be payable
	// for. A quote on an invoice about to expire sells something the bridge
	// may not be able to deliver.
	MinRemainingLife time.Duration
}

// DefaultPolicy is a starting point sized for the design's Stage 1: a $2-3k
// experiment budget gives a band of about $1,500 and a per-swap cap near $150,
// which at the measured BTCB2 price is a few thousandths of a BTC.
var DefaultPolicy = Policy{
	MinSwapMsat:       1_000_000,   // 1,000 sats
	MaxSwapMsat:       150_000_000, // 150,000 sats
	Validity:          2 * time.Minute,
	InvoiceExpiry:     10 * time.Minute,
	OutgoingCLTVLimit: 80,
	FeeFraction:       0.003,
	MinRemainingLife:  15 * time.Minute,
}

// Valid reports whether the policy can be used.
func (p Policy) Valid() error {
	switch {
	case p.MinSwapMsat == 0 || p.MaxSwapMsat < p.MinSwapMsat:
		return fmt.Errorf("%w: swap bounds %d..%d", ErrPolicy, p.MinSwapMsat,
			p.MaxSwapMsat)
	case p.Validity <= 0:
		return fmt.Errorf("%w: no quote validity", ErrPolicy)
	case p.InvoiceExpiry < p.Validity:
		return fmt.Errorf("%w: an invoice living %v cannot carry a quote "+
			"valid for %v", ErrPolicy, p.InvoiceExpiry, p.Validity)
	case p.OutgoingCLTVLimit == 0:
		return fmt.Errorf("%w: no outgoing CLTV limit", ErrPolicy)
	case p.FeeFraction < 0:
		return fmt.Errorf("%w: negative fee fraction", ErrPolicy)
	case p.MinRemainingLife < p.Validity:
		return fmt.Errorf("%w: quoting for %v against invoices with %v left",
			ErrPolicy, p.Validity, p.MinRemainingLife)
	}

	return nil
}

// Quote is what the bridge offers.
type Quote struct {
	// Hash is the payment hash of the invoice being paid, and of the hold
	// invoice created to fund it.
	Hash node.Hash

	// HoldInvoice is what the payer pays, on the incoming chain.
	HoldInvoice string

	// Outgoing is the invoice the bridge will pay with the proceeds, exactly
	// as it was supplied. The bridge never rewrites it.
	Outgoing string

	// IncomingMsat is what the payer owes, OutgoingMsat what the destination
	// receives. They are in different units.
	IncomingMsat uint64
	OutgoingMsat uint64

	// Rate is outgoing units per incoming unit, and Spread the fraction
	// charged over it.
	Rate   float64
	Spread float64

	// MaxFeeMsat is the routing budget, which comes out of the spread.
	MaxFeeMsat uint64

	// CLTVDelta is what the payer must leave on the final hop, in blocks of
	// the incoming chain.
	CLTVDelta uint32

	// Expires is when the rate stops being honoured.
	Expires time.Time

	// Description is the outgoing invoice's own, shown back to the payer.
	Description string
}

// Pricer supplies the rate to quote at, or a refusal.
type Pricer func(ctx context.Context) (rate.Reading, error)

// Rates supplies both chains' block spacing, freshly measured. Both are
// needed: the incoming CLTV is sized by comparing how long the outgoing leg
// can take against how fast the incoming chain produces blocks, and using one
// chain's spacing for both would size a swap against a chain it is not on.
type Rates func(ctx context.Context) (driver.Rates, error)

// Quoter makes quotes. Use it by pointer: it holds a lock, so a copy would
// serialise against nothing.
type Quoter struct {
	In    node.Incoming
	Out   node.Outgoing
	Store store.Store

	// Price is the rate oracle, Spacing both chains' block spacing.
	Price   Pricer
	Spacing Rates

	// Margin is the HTLC timing policy the incoming CLTV is sized against.
	Margin margin.Policy

	// Policy is the terms offered.
	Policy Policy

	// Now is the clock, for tests.
	Now func() time.Time

	mu      sync.Mutex
	quoting map[node.Hash]struct{}
}

// begin takes exclusive charge of a payment hash for the length of one quote.
//
// Without it two callers can both pass the duplicate check before either has
// written anything, and the second one's record overwrites the first's terms:
// same swap, different amount, different rate, and a hold invoice already out
// with a payer for the amount that was replaced. The node would refuse the
// second invoice for a hash it already has, so the damage is to the record
// rather than to the money, but a record that disagrees with the invoice a
// payer holds is exactly what the rest of this repository is built to avoid.
//
// This assumes one process. That is the deployment model, and the store would
// need a compare-and-set for anything else.
func (q *Quoter) begin(hash node.Hash) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.quoting == nil {
		q.quoting = map[node.Hash]struct{}{}
	}
	if _, taken := q.quoting[hash]; taken {
		return fmt.Errorf("%w: %x is being quoted now", ErrDuplicate, hash)
	}
	q.quoting[hash] = struct{}{}

	return nil
}

func (q *Quoter) end(hash node.Hash) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.quoting, hash)
}

func (q *Quoter) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}

	return time.Now()
}

// Quote prices a swap, creates the hold invoice, and records it.
//
// On success the swap is in swap.Offered and ready for the runner. On any
// refusal nothing has been created on either chain.
func (q *Quoter) Quote(ctx context.Context, invoice string) (Quote, error) {
	if err := q.Policy.Valid(); err != nil {
		return Quote{}, err
	}
	if q.In == nil || q.Out == nil || q.Store == nil || q.Price == nil ||
		q.Spacing == nil {

		return Quote{}, fmt.Errorf("%w: quoter is missing a dependency",
			ErrPolicy)
	}

	now := q.now()

	// The paying node's own decoder, not a second one in this process: a
	// parser that disagreed by one field would have the bridge hold an
	// invoice for one hash while the node paid another.
	dec, err := q.Out.Decode(ctx, invoice)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}

	if err := q.checkInvoice(dec, now); err != nil {
		return Quote{}, err
	}

	if err := q.begin(dec.Hash); err != nil {
		return Quote{}, err
	}
	defer q.end(dec.Hash)

	// A second hold invoice for a hash that already has one would let the
	// same preimage claim twice, and would leave two records fighting over
	// one swap. Refuse before anything is created.
	if existing, err := q.Store.Get(ctx, dec.Hash); err == nil {
		return Quote{}, fmt.Errorf("%w: %x is %v", ErrDuplicate, dec.Hash,
			existing.State)
	} else if !errors.Is(err, store.ErrNotFound) {
		return Quote{}, fmt.Errorf("checking for an existing swap: %w", err)
	}

	reading, err := q.Price(ctx)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}

	terms, err := q.price(dec, reading)
	if err != nil {
		return Quote{}, err
	}
	terms.Outgoing = invoice

	spacing, err := q.Spacing(ctx)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if spacing.As.IsZero() {
		return Quote{}, fmt.Errorf("%w: block spacing carries no measurement "+
			"time", ErrRefused)
	}
	blocks, err := margin.RequiredIncomingBlocks(spacing.Incoming, margin.Leg{
		Bounds: spacing.Outgoing,
		Blocks: q.Policy.OutgoingCLTVLimit,
	}, q.Margin)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: sizing the incoming CLTV: %w",
			ErrRefused, err)
	}
	terms.CLTVDelta = blocks
	terms.Expires = now.Add(q.Policy.Validity)

	if err := q.create(ctx, &terms, now); err != nil {
		return Quote{}, err
	}

	return terms, nil
}

// checkInvoice refuses what the bridge cannot safely deliver.
func (q *Quoter) checkInvoice(dec node.Decoded, now time.Time) error {
	var zero node.Hash
	if dec.Hash == zero {
		return fmt.Errorf("%w: the invoice carries no payment hash", ErrRefused)
	}
	if dec.AmountMsat == 0 {
		// An invoice naming no amount would have the bridge choose what to
		// send, and price the incoming leg against its own guess.
		return fmt.Errorf("%w: the invoice names no amount", ErrRefused)
	}
	if dec.AmountMsat < q.Policy.MinSwapMsat ||
		dec.AmountMsat > q.Policy.MaxSwapMsat {

		return fmt.Errorf("%w: %d msat is outside %d..%d", ErrRefused,
			dec.AmountMsat, q.Policy.MinSwapMsat, q.Policy.MaxSwapMsat)
	}
	if dec.Expiry.IsZero() {
		// Every well-formed invoice carries a timestamp, so no expiry means
		// something could not be read. Skipping the check instead would be
		// the permissive reading of a malformed invoice.
		return fmt.Errorf("%w: the invoice has no readable expiry", ErrRefused)
	}
	if dec.Expiry.Sub(now) < q.Policy.MinRemainingLife {
		// Quoting against an invoice about to expire sells something the
		// bridge may not be able to deliver: the payer funds the incoming
		// leg and the outgoing one can no longer be paid.
		return fmt.Errorf("%w: the invoice has %v left, less than the %v a "+
			"quote needs", ErrRefused, dec.Expiry.Sub(now).Round(time.Second),
			q.Policy.MinRemainingLife)
	}
	if dec.CLTVDelta >= q.Policy.OutgoingCLTVLimit {
		// The destination's own final-hop delta already fills the budget,
		// leaving no blocks for the route in between.
		return fmt.Errorf("%w: the destination wants %d blocks on the final "+
			"hop and the whole route budget is %d", ErrRefused, dec.CLTVDelta,
			q.Policy.OutgoingCLTVLimit)
	}

	return nil
}

// price works out what the payer owes.
func (q *Quoter) price(dec node.Decoded, r rate.Reading) (Quote, error) {
	if r.BTCPerBTCB2 <= 0 || math.IsNaN(r.BTCPerBTCB2) ||
		math.IsInf(r.BTCPerBTCB2, 0) {

		return Quote{}, fmt.Errorf("%w: unusable rate %g", ErrRefused,
			r.BTCPerBTCB2)
	}
	if r.Spread < 0 || r.Spread >= 1 {
		return Quote{}, fmt.Errorf("%w: unusable spread %g", ErrRefused,
			r.Spread)
	}

	// The routing budget is paid by the bridge out of the spread. If it is
	// the wider of the two, every swap loses money by construction however
	// well everything else works, so refuse rather than quote it.
	if q.Policy.FeeFraction >= r.Spread {
		return Quote{}, fmt.Errorf("%w: a routing budget of %.2f%% against a "+
			"spread of %.2f%% loses on every swap", ErrRefused,
			q.Policy.FeeFraction*100, r.Spread*100)
	}

	// Incoming units are worth r each in outgoing units, so the payer owes
	// the outgoing amount divided by the rate, plus the spread.
	//
	// Rounded up, always. Rounding to nearest would give away half a
	// millisatoshi on average per swap, which is nothing once and a
	// systematic leak across every swap the bridge ever does, in the one
	// direction that never corrects.
	exact := float64(dec.AmountMsat) / r.BTCPerBTCB2 * (1 + r.Spread)
	if math.IsInf(exact, 0) || math.IsNaN(exact) ||
		exact >= math.MaxUint64/2 {

		return Quote{}, fmt.Errorf("%w: %d msat at %g does not produce a "+
			"usable amount", ErrRefused, dec.AmountMsat, r.BTCPerBTCB2)
	}
	incoming := uint64(math.Ceil(exact))
	if incoming == 0 {
		return Quote{}, fmt.Errorf("%w: the incoming amount rounds to zero",
			ErrRefused)
	}

	fee := uint64(math.Ceil(float64(dec.AmountMsat) * q.Policy.FeeFraction))

	return Quote{
		Hash:         dec.Hash,
		IncomingMsat: incoming,
		OutgoingMsat: dec.AmountMsat,
		Rate:         r.BTCPerBTCB2,
		Spread:       r.Spread,
		MaxFeeMsat:   fee,
		Description:  dec.Description,
	}, nil
}

// create records the swap, then builds the hold invoice, then records that it
// exists.
//
// The record comes first for the same reason it does everywhere else: a crash
// between creating the invoice and writing it down would leave an invoice on
// the node that nothing is tracking, which a payer could fund and no one would
// ever settle or cancel. Recording first makes that recoverable — the driver
// finds a swap recorded as quoted whose invoice exists, and catches the record
// up.
func (q *Quoter) create(ctx context.Context, terms *Quote, now time.Time) error {
	rec := store.Record{
		Hash:              terms.Hash,
		State:             swap.Quoted,
		OutgoingCLTVLimit: q.Policy.OutgoingCLTVLimit,
		Invoice:           terms.Outgoing,
		IncomingMsat:      terms.IncomingMsat,
		OutgoingMsat:      terms.OutgoingMsat,
		MaxFeeMsat:        terms.MaxFeeMsat,
		QuoteExpiry:       terms.Expires,
		Rate:              terms.Rate,
		Spread:            terms.Spread,
		Created:           now,
		Updated:           now,
	}
	if err := q.Store.Put(ctx, rec); err != nil {
		return fmt.Errorf("recording the quote: %w", err)
	}

	hold, err := q.In.AddHoldInvoice(ctx, node.HoldInvoice{
		Hash:       terms.Hash,
		AmountMsat: terms.IncomingMsat,
		CLTVDelta:  terms.CLTVDelta,
		Expiry:     q.Policy.InvoiceExpiry,
		Memo:       terms.Description,
	})
	if err != nil {
		return fmt.Errorf("creating the hold invoice: %w", err)
	}
	terms.HoldInvoice = hold

	// Through the state machine rather than by assignment, so that the one
	// place that knows which transitions exist stays the only place that
	// makes them.
	next, _, err := swap.Step(rec.State, swap.InvoiceOffered)
	if err != nil {
		return fmt.Errorf("recording that the invoice exists: %w", err)
	}
	rec.State = next
	rec.Updated = q.now()
	if err := q.Store.Put(ctx, rec); err != nil {
		// The invoice exists and the record says the swap is only quoted.
		// That is recoverable and deliberately so: the driver finds a
		// quoted swap whose invoice is on the node and catches the record
		// up, rather than leaving an invoice nothing will ever cancel.
		return fmt.Errorf("recording that the invoice exists: %w", err)
	}

	return nil
}
