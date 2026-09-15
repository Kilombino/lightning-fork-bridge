package quote

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/driver"
	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

var (
	now      = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	preimage = node.Preimage{0xab, 0xcd}
	hash     = preimage.Hash()

	blake = margin.Bounds{Fast: 155 * time.Second, Slow: 1200 * time.Second}
	btc   = margin.Bounds{Fast: 300 * time.Second, Slow: 600 * time.Second}
	marg  = margin.Policy{Settlement: time.Hour, MaxIncomingBlocks: 2016}
)

// fakeIn records what the bridge asked for.
type fakeIn struct {
	asked  *node.HoldInvoice
	addErr error
}

func (f *fakeIn) AddHoldInvoice(_ context.Context, r node.HoldInvoice) (string,
	error) {

	f.asked = &r
	if f.addErr != nil {
		return "", f.addErr
	}

	return "lnblakert1holdinvoice", nil
}

func (f *fakeIn) LookupInvoice(context.Context, node.Hash) (node.Invoice,
	error) {

	return node.Invoice{}, node.ErrUnknownHash
}
func (f *fakeIn) SettleInvoice(context.Context, node.Preimage) error { return nil }
func (f *fakeIn) CancelInvoice(context.Context, node.Hash) error     { return nil }
func (f *fakeIn) BlockHeight(context.Context) (int32, error)         { return 500, nil }

// fakeOut decodes.
type fakeOut struct {
	decoded   node.Decoded
	decodeErr error
}

func (f *fakeOut) Decode(context.Context, string) (node.Decoded, error) {
	return f.decoded, f.decodeErr
}
func (f *fakeOut) Pay(context.Context, node.PayRequest) (node.Payment, error) {
	return node.Payment{}, nil
}
func (f *fakeOut) LookupPayment(context.Context, node.Hash) (node.Payment,
	error) {

	return node.Payment{}, nil
}
func (f *fakeOut) BlockHeight(context.Context) (int32, error) { return 800, nil }

// decoded is a plausible BTC invoice: 100,000 sats, an hour to run.
func decoded() node.Decoded {
	return node.Decoded{
		Hash:        hash,
		AmountMsat:  100_000_000,
		CLTVDelta:   40,
		Expiry:      now.Add(time.Hour),
		Destination: "03abc",
		Description: "a coffee",
	}
}

func reading(spread float64) rate.Reading {
	return rate.Reading{
		BTCPerBTCB2: 0.00308078, At: now, Sources: 2, Spread: spread,
	}
}

func build(t *testing.T, in *fakeIn, out *fakeOut, spread float64) (*Quoter,
	*store.Memory) {

	t.Helper()

	st := store.NewMemory()

	return &Quoter{
		In: in, Out: out, Store: st,
		Price: func(context.Context) (rate.Reading, error) {
			return reading(spread), nil
		},
		Spacing: func(context.Context) (driver.Rates, error) {
			return driver.Rates{Incoming: blake, Outgoing: btc, As: now}, nil
		},
		Room: func(ctx context.Context) (uint64, error) {
			// Plenty, less whatever is already promised, which is how the
			// daemon computes it.
			committed, err := store.Committed(ctx, st)
			if err != nil {
				return 0, err
			}
			const balance = 10_000_000_000

			return balance - committed, nil
		},
		Margin: marg,
		Policy: DefaultPolicy,
		Now:    func() time.Time { return now },
	}, st
}

// TestAQuoteIsAWholeSwap: priced, created on the incoming chain, and recorded,
// with the outgoing invoice kept so the runner can pay it.
func TestAQuoteIsAWholeSwap(t *testing.T) {
	ctx := context.Background()
	in, out := &fakeIn{}, &fakeOut{decoded: decoded()}
	q, st := build(t, in, out, 0.01)

	got, err := q.Quote(ctx, "lnbc1originalinvoice")
	if err != nil {
		t.Fatal(err)
	}

	if got.Hash != hash {
		t.Errorf("hash %x, want %x", got.Hash, hash)
	}
	if got.HoldInvoice != "lnblakert1holdinvoice" {
		t.Errorf("hold invoice %q", got.HoldInvoice)
	}
	if got.Outgoing != "lnbc1originalinvoice" {
		t.Errorf("the outgoing invoice was rewritten to %q", got.Outgoing)
	}
	if got.OutgoingMsat != 100_000_000 {
		t.Errorf("outgoing %d", got.OutgoingMsat)
	}
	if !got.Expires.Equal(now.Add(DefaultPolicy.Validity)) {
		t.Errorf("expires %v", got.Expires)
	}

	// 100,000,000 outgoing msat at 0.00308078, plus a one percent spread.
	want := uint64(math.Ceil(100_000_000 / 0.00308078 * 1.01))
	if got.IncomingMsat < want || got.IncomingMsat > want+2 {
		t.Errorf("incoming %d, want about %d", got.IncomingMsat, want)
	}

	// The hold invoice must carry the CLTV the margin policy sized, not a
	// node default, and the amount the payer was quoted.
	if in.asked == nil {
		t.Fatal("no hold invoice was created")
	}
	if in.asked.Hash != hash {
		t.Errorf("the hold invoice was built on %x", in.asked.Hash)
	}
	if in.asked.AmountMsat != got.IncomingMsat {
		t.Errorf("the invoice asks %d, the quote says %d",
			in.asked.AmountMsat, got.IncomingMsat)
	}
	if in.asked.CLTVDelta != got.CLTVDelta || got.CLTVDelta == 0 {
		t.Errorf("CLTV delta %d vs %d", in.asked.CLTVDelta, got.CLTVDelta)
	}
	if in.asked.Expiry != DefaultPolicy.InvoiceExpiry {
		t.Errorf("invoice expiry %v", in.asked.Expiry)
	}

	// The record has to be ready for the runner without anything else
	// happening first.
	rec, err := st.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != swap.Offered {
		t.Errorf("recorded as %v, want offered", rec.State)
	}
	if rec.Invoice != "lnbc1originalinvoice" {
		t.Errorf("the record does not hold the invoice to pay: %q", rec.Invoice)
	}
	if rec.OutgoingCLTVLimit != DefaultPolicy.OutgoingCLTVLimit {
		t.Errorf("CLTV limit %d", rec.OutgoingCLTVLimit)
	}
	if rec.MaxFeeMsat != got.MaxFeeMsat || rec.MaxFeeMsat == 0 {
		t.Errorf("fee budget %d vs %d", rec.MaxFeeMsat, got.MaxFeeMsat)
	}
	if !rec.QuoteExpiry.Equal(got.Expires) {
		t.Errorf("quote expiry %v vs %v", rec.QuoteExpiry, got.Expires)
	}
}

// TestTheIncomingCLTVComesFromBothChains. Sizing it against the incoming
// chain's spacing alone would compare a duration on one chain to a block count
// on another, which is the mistake the whole margin package exists to prevent.
func TestTheIncomingCLTVComesFromBothChains(t *testing.T) {
	ctx := context.Background()

	fast, err := margin.RequiredIncomingBlocks(blake, margin.Leg{
		Bounds: btc, Blocks: DefaultPolicy.OutgoingCLTVLimit,
	}, marg)
	if err != nil {
		t.Fatal(err)
	}

	q, _ := build(t, &fakeIn{}, &fakeOut{decoded: decoded()}, 0.01)
	got, err := q.Quote(ctx, "lnbc1x")
	if err != nil {
		t.Fatal(err)
	}
	if got.CLTVDelta != fast {
		t.Errorf("CLTV delta %d, want %d", got.CLTVDelta, fast)
	}

	// A slower outgoing chain needs more incoming blocks. If the quote were
	// using one chain's spacing for both, this would not move.
	slower := margin.Bounds{Fast: 600 * time.Second, Slow: 3600 * time.Second}
	q2, _ := build(t, &fakeIn{}, &fakeOut{decoded: decoded()}, 0.01)
	q2.Spacing = func(context.Context) (driver.Rates, error) {
		return driver.Rates{Incoming: blake, Outgoing: slower, As: now}, nil
	}
	got2, err := q2.Quote(ctx, "lnbc1x")
	if err != nil {
		t.Fatal(err)
	}
	if got2.CLTVDelta <= got.CLTVDelta {
		t.Errorf("a slower outgoing chain gave %d blocks, not more than %d",
			got2.CLTVDelta, got.CLTVDelta)
	}
}

// TestRoundingNeverFavoursThePayer. Half a millisatoshi is nothing once and a
// systematic leak across every swap a bridge ever does, in a direction that
// never corrects.
func TestRoundingNeverFavoursThePayer(t *testing.T) {
	ctx := context.Background()

	for _, amount := range []uint64{
		1_000_001, 3_333_333, 7_777_777, 99_999_999, 100_000_001,
	} {
		dec := decoded()
		dec.AmountMsat = amount
		q, _ := build(t, &fakeIn{}, &fakeOut{decoded: dec}, 0.01)

		got, err := q.Quote(ctx, "lnbc1x")
		if err != nil {
			t.Fatalf("%d: %v", amount, err)
		}

		exact := float64(amount) / 0.00308078 * 1.01
		if float64(got.IncomingMsat) < exact {
			t.Errorf("%d msat: quoted %d, which is under the exact %f",
				amount, got.IncomingMsat, exact)
		}
		if float64(got.IncomingMsat) >= exact+1 {
			t.Errorf("%d msat: quoted %d, more than a msat over %f",
				amount, got.IncomingMsat, exact)
		}
	}
}

// TestARoutingBudgetWiderThanTheSpreadIsRefused. Every such swap loses money by
// construction however well everything else works.
func TestARoutingBudgetWiderThanTheSpreadIsRefused(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name   string
		spread float64
		refuse bool
	}{
		{"a spread well over the budget", 0.01, false},
		{"a spread equal to the budget", DefaultPolicy.FeeFraction, true},
		{"a spread under the budget", 0.001, true},
		{"no spread at all", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := &fakeIn{}
			q, st := build(t, in, &fakeOut{decoded: decoded()}, c.spread)

			_, err := q.Quote(ctx, "lnbc1x")
			if c.refuse {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("want ErrRefused, got %v", err)
				}
				if in.asked != nil {
					t.Error("it created an invoice for a swap it refused")
				}
				if p, _ := st.Pending(ctx); len(p) != 0 {
					t.Error("it recorded a swap it refused")
				}

				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestInvoicesTheBridgeWillNotTouch.
func TestInvoicesTheBridgeWillNotTouch(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name string
		with func(d *node.Decoded)
	}{
		{
			// The bridge would be choosing what to send, and pricing the
			// incoming leg against its own guess.
			"one that names no amount",
			func(d *node.Decoded) { d.AmountMsat = 0 },
		},
		{
			// The payer funds the incoming leg and the outgoing one can no
			// longer be paid.
			"one about to expire",
			func(d *node.Decoded) { d.Expiry = now.Add(time.Minute) },
		},
		{"one already expired",
			func(d *node.Decoded) { d.Expiry = now.Add(-time.Minute) }},
		{"one below the minimum",
			func(d *node.Decoded) { d.AmountMsat = 1 }},
		{"one above the per-swap cap",
			func(d *node.Decoded) { d.AmountMsat = 10_000_000_000 }},
		{
			// The destination's own final-hop delta fills the whole route
			// budget, leaving nothing for the hops in between.
			"one whose final hop wants the whole budget",
			func(d *node.Decoded) {
				d.CLTVDelta = DefaultPolicy.OutgoingCLTVLimit
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dec := decoded()
			c.with(&dec)
			in := &fakeIn{}
			q, st := build(t, in, &fakeOut{decoded: dec}, 0.01)

			if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrRefused) {
				t.Fatalf("want ErrRefused, got %v", err)
			}
			if in.asked != nil {
				t.Error("it created an invoice anyway")
			}
			if p, _ := st.Pending(ctx); len(p) != 0 {
				t.Error("it recorded the swap anyway")
			}
		})
	}
}

// TestASecondQuoteForTheSameHashIsRefused. Two hold invoices for one payment
// hash would let one preimage claim twice and leave two records fighting over
// one swap.
func TestASecondQuoteForTheSameHashIsRefused(t *testing.T) {
	ctx := context.Background()
	in := &fakeIn{}
	q, st := build(t, in, &fakeOut{decoded: decoded()}, 0.01)

	if _, err := q.Quote(ctx, "lnbc1x"); err != nil {
		t.Fatal(err)
	}
	created := in.asked

	if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	if in.asked != created {
		t.Error("a second hold invoice was created for the same hash")
	}

	// It stays refused once the swap has finished, because the preimage for
	// a settled hash is public and a new invoice on it could be claimed by
	// anyone who saw the first.
	rec, _ := st.Get(ctx, hash)
	rec.State = swap.Settled
	rec.Preimage = preimage
	if err := st.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("want ErrDuplicate for a settled hash, got %v", err)
	}
}

// TestNothingIsQuotedWithoutAFreshMeasurement.
func TestNothingIsQuotedWithoutAFreshMeasurement(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name string
		with func(q *Quoter)
	}{
		{"no usable rate", func(q *Quoter) {
			q.Price = func(context.Context) (rate.Reading, error) {
				return rate.Reading{}, rate.ErrBroken
			}
		}},
		{"no usable block spacing", func(q *Quoter) {
			q.Spacing = func(context.Context) (driver.Rates, error) {
				return driver.Rates{}, errors.New("not enough samples")
			}
		}},
		{
			// Bounds with no measurement time could be any age, and a swap
			// sized against a block rate the chain has left is the failure
			// the margin policy exists to prevent.
			"spacing that carries no measurement time", func(q *Quoter) {
				q.Spacing = func(context.Context) (driver.Rates, error) {
					return driver.Rates{Incoming: blake, Outgoing: btc}, nil
				}
			},
		},
		{"a rate of zero", func(q *Quoter) {
			q.Price = func(context.Context) (rate.Reading, error) {
				r := reading(0.01)
				r.BTCPerBTCB2 = 0

				return r, nil
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := &fakeIn{}
			q, st := build(t, in, &fakeOut{decoded: decoded()}, 0.01)
			c.with(q)

			if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrRefused) {
				t.Fatalf("want ErrRefused, got %v", err)
			}
			if in.asked != nil {
				t.Error("it created an invoice anyway")
			}
			if p, _ := st.Pending(ctx); len(p) != 0 {
				t.Error("it recorded the swap anyway")
			}
		})
	}
}

// TestAnInvoiceCreatedButNotRecordedIsRecoverable. The record is written
// before the invoice is created, so a crash in between leaves a swap recorded
// as quoted whose invoice exists on the node, which the driver catches up
// rather than leaving an invoice nothing will ever cancel.
func TestAnInvoiceCreatedButNotRecordedIsRecoverable(t *testing.T) {
	ctx := context.Background()
	in := &fakeIn{addErr: errors.New("the node went away")}
	q, st := build(t, in, &fakeOut{decoded: decoded()}, 0.01)

	if _, err := q.Quote(ctx, "lnbc1x"); err == nil {
		t.Fatal("a failed invoice creation reported success")
	}

	// The record exists at Quoted, which is what makes it recoverable.
	rec, err := st.Get(ctx, hash)
	if err != nil {
		t.Fatalf("nothing was recorded, so nothing will ever clean up: %v", err)
	}
	if rec.State != swap.Quoted {
		t.Errorf("recorded as %v", rec.State)
	}
	if rec.Invoice != "lnbc1x" {
		t.Errorf("the invoice to pay was not recorded: %q", rec.Invoice)
	}
}

func TestPolicyRefusesWhatCannotWork(t *testing.T) {
	good := DefaultPolicy

	for _, c := range []struct {
		name string
		with func(p *Policy)
	}{
		{"no minimum", func(p *Policy) { p.MinSwapMsat = 0 }},
		{"a maximum under the minimum",
			func(p *Policy) { p.MaxSwapMsat = p.MinSwapMsat - 1 }},
		{"no validity", func(p *Policy) { p.Validity = 0 }},
		{
			// A quote that outlives the invoice it is for would be honoured
			// against something the payer can no longer pay.
			"an invoice shorter lived than the quote",
			func(p *Policy) { p.InvoiceExpiry = p.Validity - time.Second },
		},
		{"no outgoing CLTV limit",
			func(p *Policy) { p.OutgoingCLTVLimit = 0 }},
		{"a negative fee fraction", func(p *Policy) { p.FeeFraction = -1 }},
		{
			// Quoting for longer than an invoice is required to have left
			// means accepting money for something that may already be gone.
			"quoting for longer than the invoices need to live",
			func(p *Policy) { p.MinRemainingLife = p.Validity - time.Second },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.with(&p)
			if err := p.Valid(); !errors.Is(err, ErrPolicy) {
				t.Errorf("want ErrPolicy, got %v", err)
			}
		})
	}

	if err := good.Valid(); err != nil {
		t.Errorf("the default policy is not valid: %v", err)
	}
}

func TestAQuoterMissingADependencyRefuses(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name string
		with func(q *Quoter)
	}{
		{"no incoming node", func(q *Quoter) { q.In = nil }},
		{"no outgoing node", func(q *Quoter) { q.Out = nil }},
		{"no store", func(q *Quoter) { q.Store = nil }},
		{"no pricer", func(q *Quoter) { q.Price = nil }},
		{"no spacing", func(q *Quoter) { q.Spacing = nil }},
		{"no headroom", func(q *Quoter) { q.Room = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Built fresh rather than copied: a Quoter holds a mutex, so
			// copying one is a bug go vet catches.
			q, _ := build(t, &fakeIn{}, &fakeOut{decoded: decoded()}, 0.01)
			c.with(q)
			if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrPolicy) {
				t.Errorf("want ErrPolicy, got %v", err)
			}
		})
	}
}

// TestConcurrentQuotesForOneHash. Two callers racing the same invoice can both
// pass the duplicate check before either has written anything. The loser must
// be refused, not allowed to overwrite the winner's terms with its own amount
// and rate while a payer holds an invoice for the amount that was replaced.
func TestConcurrentQuotesForOneHash(t *testing.T) {
	ctx := context.Background()

	in := &slowIn{}
	q := &Quoter{
		In: in, Out: &fakeOut{decoded: decoded()}, Store: store.NewMemory(),
		Price: func(context.Context) (rate.Reading, error) {
			return reading(0.01), nil
		},
		Spacing: func(context.Context) (driver.Rates, error) {
			return driver.Rates{Incoming: blake, Outgoing: btc, As: now}, nil
		},
		Room: func(context.Context) (uint64, error) {
			return 10_000_000_000, nil
		},
		Margin: marg, Policy: DefaultPolicy,
		Now: func() time.Time { return now },
	}

	const racers = 16
	var (
		wg   sync.WaitGroup
		won  atomic.Int32
		lost atomic.Int32
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := q.Quote(ctx, "lnbc1x"); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrDuplicate):
				lost.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if won.Load() != 1 {
		t.Errorf("%d quotes succeeded, want exactly one", won.Load())
	}
	if lost.Load() != racers-1 {
		t.Errorf("%d were refused, want %d", lost.Load(), racers-1)
	}
	if n := in.created.Load(); n != 1 {
		t.Errorf("%d hold invoices were created for one payment hash", n)
	}
}

// slowIn holds the invoice creation open long enough for the race to be real.
type slowIn struct {
	created atomic.Int32
}

func (s *slowIn) AddHoldInvoice(context.Context, node.HoldInvoice) (string,
	error) {

	s.created.Add(1)
	time.Sleep(5 * time.Millisecond)

	return "lnblakert1holdinvoice", nil
}

func (s *slowIn) LookupInvoice(context.Context, node.Hash) (node.Invoice,
	error) {

	return node.Invoice{}, node.ErrUnknownHash
}
func (s *slowIn) SettleInvoice(context.Context, node.Preimage) error { return nil }
func (s *slowIn) CancelInvoice(context.Context, node.Hash) error     { return nil }
func (s *slowIn) BlockHeight(context.Context) (int32, error)         { return 500, nil }

// TestAnInvoiceWithNoReadablePaymentHashOrExpiry is refused before anything is
// created. A missing hash cannot identify a swap, and a missing expiry means
// the invoice could not be read, which is not the same as one that never
// expires.
func TestAnInvoiceWithNoReadablePaymentHashOrExpiry(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name string
		with func(d *node.Decoded)
	}{
		{"no payment hash", func(d *node.Decoded) { d.Hash = node.Hash{} }},
		{"no expiry", func(d *node.Decoded) { d.Expiry = time.Time{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dec := decoded()
			c.with(&dec)
			in := &fakeIn{}
			q, st := build(t, in, &fakeOut{decoded: dec}, 0.01)

			if _, err := q.Quote(ctx, "lnbc1x"); !errors.Is(err, ErrRefused) {
				t.Fatalf("want ErrRefused, got %v", err)
			}
			if in.asked != nil {
				t.Error("it created an invoice anyway")
			}
			if p, _ := st.Pending(ctx); len(p) != 0 {
				t.Error("it recorded the swap anyway")
			}
		})
	}
}

// TestADecodeFailureStopsEverything. If the paying node cannot read the
// invoice, nothing downstream has a payment hash it can trust.
func TestADecodeFailureStopsEverything(t *testing.T) {
	ctx := context.Background()
	in := &fakeIn{}
	q, st := build(t, in, &fakeOut{
		decodeErr: errors.New("checksum failed"),
	}, 0.01)

	if _, err := q.Quote(ctx, "lnbc1garbage"); !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
	if in.asked != nil {
		t.Error("it created an invoice for something it could not read")
	}
	if p, _ := st.Pending(ctx); len(p) != 0 {
		t.Error("it recorded a swap for something it could not read")
	}
}

// TestTheBridgeCannotOversellItsBalance is the gap that inventory pricing
// exposed: a per-swap cap says nothing about how many swaps are outstanding.
//
// Three swaps each comfortably inside MaxSwapMsat can still, together, promise
// more than the bridge holds. The balance says what could be sent right now;
// it says nothing about what is already spoken for. Discovering that at the
// end means an incoming HTLC locked in and no way to pay the outgoing leg.
func TestTheBridgeCannotOversellItsBalance(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	// Enough for two swaps of 40,000,000 msat and not a third.
	const balance = 100_000_000

	build := func(hash node.Hash) *Quoter {
		dec := decoded()
		dec.Hash = hash
		dec.AmountMsat = 40_000_000

		return &Quoter{
			In: &fakeIn{}, Out: &fakeOut{decoded: dec}, Store: st,
			Price: func(context.Context) (rate.Reading, error) {
				return reading(0.01), nil
			},
			Spacing: func(context.Context) (driver.Rates, error) {
				return driver.Rates{Incoming: blake, Outgoing: btc,
					As: now}, nil
			},
			Room: func(ctx context.Context) (uint64, error) {
				committed, err := store.Committed(ctx, st)
				if err != nil {
					return 0, err
				}

				return balance - committed, nil
			},
			Margin: marg, Policy: DefaultPolicy,
			Now: func() time.Time { return now },
		}
	}

	for i, h := range []node.Hash{{1}, {2}} {
		if _, err := build(h).Quote(ctx, "lnbc1x"); err != nil {
			t.Fatalf("swap %d was refused with room to spare: %v", i+1, err)
		}
	}

	// The third is inside the per-swap cap and outside what is left.
	third := node.Hash{3}
	_, err := build(third).Quote(ctx, "lnbc1x")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("the bridge oversold its balance: %v", err)
	}
	if _, err := st.Get(ctx, third); !errors.Is(err, store.ErrNotFound) {
		t.Error("it recorded the swap it could not cover")
	}

	// Once one finishes, its commitment is released and there is room again.
	done, _ := st.Get(ctx, node.Hash{1})
	done.State = swap.Settled
	if err := st.Put(ctx, done); err != nil {
		t.Fatal(err)
	}
	if _, err := build(third).Quote(ctx, "lnbc1x"); err != nil {
		t.Errorf("room was not released when a swap finished: %v", err)
	}
}
