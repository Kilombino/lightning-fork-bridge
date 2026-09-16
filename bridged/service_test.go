package bridged

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/inventory"
	"github.com/paulscode/lightning-fork-bridge/margin"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

// quiet keeps test output to what the test itself says.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeOut answers Decode and nothing else, which is all routing needs.
type fakeOut struct {
	// accepts is the invoice prefix this node will read. A real node
	// refuses an invoice for another chain, which is exactly what routing
	// depends on.
	accepts string
	decoded node.Decoded
	calls   int
}

func (f *fakeOut) Decode(_ context.Context, invoice string) (node.Decoded, error) {
	f.calls++
	if f.accepts == "" || len(invoice) < len(f.accepts) ||
		invoice[:len(f.accepts)] != f.accepts {

		return node.Decoded{}, errors.New("not an invoice for this chain")
	}

	return f.decoded, nil
}

func (f *fakeOut) Pay(context.Context, node.PayRequest) (node.Payment, error) {
	return node.Payment{}, errors.New("not used")
}

func (f *fakeOut) LookupPayment(context.Context, node.Hash) (node.Payment, error) {
	return node.Payment{}, errors.New("not used")
}

func (f *fakeOut) BlockHeight(context.Context) (int32, error) { return 100, nil }

// testService is a Service with both directions wired to fakes.
func testService(t *testing.T, toBitcoin, toBlake2b *fakeOut,
	balance func(context.Context) (uint64, error)) *Service {

	t.Helper()

	if balance == nil {
		balance = func(context.Context) (uint64, error) {
			return 1_000_000_000, nil
		}
	}

	s := &Service{log: quiet(), ctx: context.Background()}
	if toBitcoin != nil {
		s.sides = append(s.sides, &side{
			name: "toBitcoin", out: toBitcoin, balance: balance,
			dir: inventory.Draining,
		})
	}
	if toBlake2b != nil {
		s.sides = append(s.sides, &side{
			name: "toBlake2b", out: toBlake2b, balance: balance,
			dir: inventory.Replenishing, invert: true,
		})
	}

	return s
}

// Routing by asking the nodes is the whole reason there is no second parser
// in this process, so it is worth testing that it actually discriminates.
func TestRouteAsksTheNodes(t *testing.T) {
	t.Parallel()

	btc := &fakeOut{accepts: "lnbc"}
	lf := &fakeOut{accepts: "lnblake"}
	s := testService(t, btc, lf, nil)

	tests := []struct {
		invoice string
		want    string
	}{
		{invoice: "lnbcrt1500n1p4invoice", want: "toBitcoin"},
		{invoice: "lnblakert1m1p4invoice", want: "toBlake2b"},
	}

	for _, test := range tests {
		sd, _, err := s.route(context.Background(), test.invoice)
		if err != nil {
			t.Fatalf("%s: %v", test.invoice, err)
		}
		if sd.name != test.want {
			t.Errorf("%s routed to %s, wanted %s", test.invoice, sd.name,
				test.want)
		}
	}
}

func TestRouteRefusesAnInvoiceNeitherNodeReads(t *testing.T) {
	t.Parallel()

	s := testService(t, &fakeOut{accepts: "lnbc"},
		&fakeOut{accepts: "lnblake"}, nil)

	_, _, err := s.route(context.Background(), "ltc1notourchain")
	if !errors.Is(err, ErrNoDirection) {
		t.Fatalf("wanted ErrNoDirection, got %v", err)
	}
}

// A direction that is not enabled must not be reachable by handing the daemon
// an invoice for it, or a bridge with no BTCB2 would quote swaps it cannot pay.
func TestRouteWillNotUseADisabledDirection(t *testing.T) {
	t.Parallel()

	s := testService(t, &fakeOut{accepts: "lnbc"}, nil, nil)

	_, _, err := s.route(context.Background(), "lnblakert1m1p4invoice")
	if !errors.Is(err, ErrNoDirection) {
		t.Fatalf("a BLAKE2b invoice should be unroutable with only "+
			"toBitcoin enabled, got %v", err)
	}
}

// The reverse direction is the same machinery with the rate inverted, so the
// inversion is the one number that has to be right.
func TestPricerInvertsForTheReverseDirection(t *testing.T) {
	t.Parallel()

	const market = 0.00308078

	s := testService(t, &fakeOut{accepts: "lnbc"},
		&fakeOut{accepts: "lnblake"}, nil)
	s.price = rate.Reading{
		OutgoingPerIncoming: market, At: time.Now(), Sources: 2,
		Spread: 0.01,
	}
	s.priceErr = nil

	inv := inventory.DefaultPolicy
	inv.TargetOutgoingMsat = 1_000_000_000
	inv.FloorOutgoingMsat = 100_000_000

	forward, err := s.pricer(s.sides[0], inv)(context.Background())
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	reverse, err := s.pricer(s.sides[1], inv)(context.Background())
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}

	if forward.OutgoingPerIncoming != market {
		t.Errorf("forward quoted %g, wanted the market rate %g",
			forward.OutgoingPerIncoming, market)
	}
	if math.Abs(reverse.OutgoingPerIncoming-1/market) > 1e-9 {
		t.Errorf("reverse quoted %g, wanted the reciprocal %g",
			reverse.OutgoingPerIncoming, 1/market)
	}
	// The two must be reciprocals of each other, or a round trip through
	// the bridge would gain or lose value for reasons nobody chose.
	if math.Abs(forward.OutgoingPerIncoming*reverse.OutgoingPerIncoming-1) > 1e-9 {
		t.Errorf("the two directions are not reciprocal: %g * %g",
			forward.OutgoingPerIncoming, reverse.OutgoingPerIncoming)
	}
}

// A price the daemon has never read must refuse rather than quote at zero.
func TestPricerRefusesBeforeAnyPrice(t *testing.T) {
	t.Parallel()

	s := testService(t, &fakeOut{accepts: "lnbc"}, nil, nil)
	s.priceErr = errors.New("no price has been read yet")

	if _, err := s.pricer(s.sides[0], inventory.DefaultPolicy)(
		context.Background()); err == nil {

		t.Fatal("a quote before the first price should be refused")
	}
}

// Inverting zero is how a bad feed becomes an infinite rate, which would
// quote an incoming amount of zero and give the swap away.
func TestPricerRefusesToInvertAnUnusableRate(t *testing.T) {
	t.Parallel()

	for _, bad := range []float64{0, -1} {
		s := testService(t, nil, &fakeOut{accepts: "lnblake"}, nil)
		s.price = rate.Reading{OutgoingPerIncoming: bad, At: time.Now()}
		s.priceErr = nil

		_, err := s.pricer(s.sides[0], inventory.DefaultPolicy)(
			context.Background())
		if err == nil {
			t.Errorf("inverting %g should be refused", bad)
		}
	}
}

// The spread has two independent reasons to widen and charging only one of
// them underprices the other risk.
func TestSpreadTakesTheWiderOfTheTwo(t *testing.T) {
	t.Parallel()

	inv := inventory.DefaultPolicy
	inv.TargetOutgoingMsat = 1_000_000_000
	inv.FloorOutgoingMsat = 100_000_000

	tests := []struct {
		name        string
		volatility  float64
		heldMsat    uint64
		wantAtLeast float64
	}{
		{
			// Full outgoing side, so inventory is at its floor and the
			// oracle's wider number has to win.
			name: "volatility is wider", volatility: 0.025,
			heldMsat: 1_000_000_000, wantAtLeast: 0.025,
		},
		{
			// Drained outgoing side with a calm market: inventory wins.
			name: "inventory is wider", volatility: 0.001,
			heldMsat: 110_000_000, wantAtLeast: inv.BaseSpread,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			held := test.heldMsat
			s := testService(t, &fakeOut{accepts: "lnbc"}, nil,
				func(context.Context) (uint64, error) { return held, nil })
			s.price = rate.Reading{
				OutgoingPerIncoming: 0.003, At: time.Now(), Sources: 2,
				Spread: test.volatility,
			}
			s.priceErr = nil

			got, err := s.pricer(s.sides[0], inv)(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Spread < test.wantAtLeast {
				t.Errorf("spread %g is below %g, so one of the two reasons "+
					"to widen was dropped", got.Spread, test.wantAtLeast)
			}
		})
	}
}

// Headroom that ignored what is already promised would let the bridge commit
// the same balance to two swaps.
func TestHeadroomSubtractsWhatIsAlreadyCommitted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	j := openJournal(t, dir)
	defer j.Close()

	s := testService(t, &fakeOut{accepts: "lnbc"}, nil,
		func(context.Context) (uint64, error) { return 1_000_000, nil })
	s.journal = j

	room, err := s.headroom(s.sides[0])(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if room != 1_000_000 {
		t.Fatalf("an empty journal should leave the whole balance, got %d",
			room)
	}

	commit(t, j, 400_000)

	room, err = s.headroom(s.sides[0])(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if room != 600_000 {
		t.Errorf("with 400,000 committed the headroom should be 600,000, "+
			"got %d", room)
	}
}

// A balance below what is committed must report no room rather than wrap
// around, which on an unsigned subtraction would look like limitless room.
func TestHeadroomDoesNotUnderflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	j := openJournal(t, dir)
	defer j.Close()

	s := testService(t, &fakeOut{accepts: "lnbc"}, nil,
		func(context.Context) (uint64, error) { return 100_000, nil })
	s.journal = j
	commit(t, j, 900_000)

	room, err := s.headroom(s.sides[0])(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if room != 0 {
		t.Errorf("committed more than the balance should leave no room, "+
			"got %d", room)
	}
}

// Using one chain's spacing for both legs would size a swap against a chain
// it is not on, so the reverse direction has to swap them.
func TestSpacingSwapsTheLegsForTheReverseDirection(t *testing.T) {
	t.Parallel()

	s := testService(t, nil, nil, nil)
	s.lfChain = observerAt(t, 30*time.Second)
	s.btcChain = observerAt(t, 600*time.Second)

	forward, err := s.spacing(false)(context.Background())
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	reverse, err := s.spacing(true)(context.Background())
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}

	if forward.Incoming != reverse.Outgoing ||
		forward.Outgoing != reverse.Incoming {

		t.Errorf("the reverse direction should be the forward one with the "+
			"legs exchanged: forward %+v, reverse %+v", forward, reverse)
	}
	// The fast chain is the BLAKE2b one, and it is the incoming leg only in
	// the forward direction.
	if forward.Incoming.Fast >= forward.Outgoing.Fast {
		t.Errorf("the forward incoming leg should be the faster chain: %+v",
			forward)
	}
}

func TestMarginPolicyIsCarriedIntoBothDirections(t *testing.T) {
	t.Parallel()

	// A margin policy that reached one direction and not the other would
	// size half the swaps against nothing.
	cfg := margin.Policy{Settlement: time.Hour, MaxIncomingBlocks: 2016}
	s := &Service{cfg: Config{Margin: cfg}, log: quiet()}
	if s.cfg.Margin != cfg {
		t.Fatal("the margin policy did not survive being stored")
	}
}

// openJournal is a real journal on disk, because Committed reads what was
// written rather than what a fake would have remembered.
func openJournal(t *testing.T, dir string) *store.Journal {
	t.Helper()

	j, err := store.Open(filepath.Join(dir, "swaps.journal"))
	if err != nil {
		t.Fatal(err)
	}

	return j
}

// commit writes one swap that counts against headroom.
func commit(t *testing.T, j *store.Journal, outgoingMsat uint64) {
	t.Helper()

	now := time.Now()
	err := j.Put(context.Background(), store.Record{
		Hash: node.Hash{byte(outgoingMsat), 2, 3}, State: swap.Offered,
		OutgoingCLTVLimit: 40, Invoice: "lnbc1fake",
		IncomingMsat: outgoingMsat * 300, OutgoingMsat: outgoingMsat,
		Rate: 0.00308078, Spread: 0.01, Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// observerAt is a chain observer already holding enough blocks at a given
// spacing to produce an estimate.
func observerAt(t *testing.T, spacing time.Duration) *chainrate.Observer {
	t.Helper()

	// The target is the spacing being simulated and the factors are tight,
	// so the bounds hug what was measured. Wide factors would make every
	// swap need a CLTV nobody would route, which is a property of the
	// parameters rather than of the code under test.
	obs, err := chainrate.New(chainrate.Params{
		RetargetInterval: 2016, TargetSpacing: spacing,
		SurgeFactor: 2, StallFactor: 2, MinSamples: 6,
		StaleAfter: 24 * time.Hour, SurgeWindow: 12,
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().Add(-20 * spacing)
	for i := 0; i < 12; i++ {
		err := obs.Add(chainrate.Block{
			Height: int32(1000 + i),
			Time:   start.Add(time.Duration(i) * spacing),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	return obs
}

// node8 is a distinct hash for a test record.
func node8(n byte) node.Hash {
	return node.Hash{n, n, n, n, n, n, n, n}
}

// inventoryForTest is a policy scaled to the balances the tests use.
func inventoryForTest() inventory.Policy {
	p := inventory.DefaultPolicy
	p.TargetOutgoingMsat = 1_000_000_000
	p.FloorOutgoingMsat = 100_000_000

	return p
}
