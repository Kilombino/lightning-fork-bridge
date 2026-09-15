package rate

import (
	"errors"
	"math"
	"testing"
	"time"
)

var now = time.Unix(1_700_000_000, 0)

func obs(source string, price float64, at time.Time) Observation {
	return Observation{Source: source, BTCPerBTCB2: price, At: at}
}

// TestTheRealCrossCheck uses the figures measured on 2026-09-14. The venue's
// direct BTCB2/BTC pair and its dollar pair divided by a mainstream BTC/USD
// agreed to under one percent, which is what makes the check worth having.
func TestTheRealCrossCheck(t *testing.T) {
	const (
		directBTC = 0.00308078 // BTCB2_BTC last
		btcb2USD  = 239.18713133
		btcUSD    = 77638.0 // mainstream BTC/USD
	)
	derived, err := Derive("neoxa BTCB2_USDC / BTC-USD", btcb2USD, btcUSD, now)
	if err != nil {
		t.Fatal(err)
	}

	gap := math.Abs(derived.BTCPerBTCB2-directBTC) / directBTC
	if gap > 0.01 {
		t.Fatalf("the two readings differ by %.2f%%; the design document "+
			"claims under one percent", gap*100)
	}

	o, err := New(DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	r, err := o.Update(now, []Observation{
		obs("neoxa BTCB2_BTC", directBTC, now), derived,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Sources != 2 {
		t.Errorf("used %d sources, want 2", r.Sources)
	}
	if r.BTCPerBTCB2 < directBTC*0.99 || r.BTCPerBTCB2 > directBTC*1.01 {
		t.Errorf("combined to %g, want about %g", r.BTCPerBTCB2, directBTC)
	}
}

// TestDisagreementStopsQuoting is the point of the cross-check. Two readings
// that should match and do not mean one is wrong, and there is no way to tell
// which, so neither may be used.
func TestDisagreementStopsQuoting(t *testing.T) {
	o, _ := New(DefaultPolicy)

	_, err := o.Update(now, []Observation{
		obs("direct", 0.00308, now),
		obs("derived", 0.00400, now), // 30% out
	})
	if !errors.Is(err, ErrDisagreement) {
		t.Fatalf("want ErrDisagreement, got %v", err)
	}

	// Inside the tolerance it is fine.
	if _, err := o.Update(now, []Observation{
		obs("direct", 0.00308, now),
		obs("derived", 0.00314, now), // about 2%
	}); err != nil {
		t.Fatalf("sources within tolerance: %v", err)
	}
}

func TestStaleObservationsAreDropped(t *testing.T) {
	o, _ := New(DefaultPolicy)

	// One fresh, one far too old: the old one is ignored rather than
	// dragging the answer.
	r, err := o.Update(now, []Observation{
		obs("fresh", 0.00308, now),
		obs("ancient", 0.00100, now.Add(-time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Sources != 1 {
		t.Errorf("used %d sources, want 1", r.Sources)
	}
	if r.BTCPerBTCB2 != 0.00308 {
		t.Errorf("got %g, want the fresh reading", r.BTCPerBTCB2)
	}

	// Everything stale is a refusal, not a guess.
	o2, _ := New(DefaultPolicy)
	_, err = o2.Update(now, []Observation{
		obs("old", 0.00308, now.Add(-time.Hour)),
	})
	if !errors.Is(err, ErrStale) {
		t.Errorf("want ErrStale, got %v", err)
	}

	// Nothing at all is also stale rather than a panic.
	if _, err := o2.Update(now, nil); !errors.Is(err, ErrStale) {
		t.Errorf("no observations: want ErrStale, got %v", err)
	}
}

// TestFutureObservationsAreDropped: a source with a wrong clock must not be
// able to pin the oracle to a price forever.
func TestFutureObservationsAreDropped(t *testing.T) {
	o, _ := New(DefaultPolicy)
	_, err := o.Update(now, []Observation{
		obs("fast clock", 0.00308, now.Add(time.Hour)),
	})
	if !errors.Is(err, ErrStale) {
		t.Errorf("want ErrStale, got %v", err)
	}
}

// TestBreakerTripsAndStaysTripped covers the move that stops quoting, and the
// cooldown that keeps it stopped rather than flapping.
func TestBreakerTripsAndStaysTripped(t *testing.T) {
	p := DefaultPolicy
	p.BreakerMove = 0.20
	p.BreakerWindow = 10 * time.Minute
	p.Cooldown = 30 * time.Minute
	o, _ := New(p)

	at := now
	if _, err := o.Update(at, []Observation{obs("s", 0.00300, at)}); err != nil {
		t.Fatal(err)
	}

	// A 25% jump inside the window.
	at = at.Add(time.Minute)
	_, err := o.Update(at, []Observation{obs("s", 0.00375, at)})
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}

	broken, why := o.Broken(at)
	if !broken || why == "" {
		t.Errorf("breaker should report itself tripped with a reason, got %v %q",
			broken, why)
	}

	// Still tripped well inside the cooldown, even with a calm price.
	at = at.Add(20 * time.Minute)
	if _, err := o.Update(at, []Observation{obs("s", 0.00300, at)}); !errors.Is(err, ErrBroken) {
		t.Errorf("inside the cooldown: want ErrBroken, got %v", err)
	}

	// And quoting resumes after it.
	at = at.Add(15 * time.Minute)
	if _, err := o.Update(at, []Observation{obs("s", 0.00300, at)}); err != nil {
		t.Errorf("after the cooldown: %v", err)
	}
}

// TestSlowDriftDoesNotTrip: the breaker looks at a window, so a move spread
// over hours must not fire it.
func TestSlowDriftDoesNotTrip(t *testing.T) {
	p := DefaultPolicy
	p.BreakerMove = 0.20
	p.BreakerWindow = 10 * time.Minute
	o, _ := New(p)

	at := now
	price := 0.00300
	for i := 0; i < 40; i++ {
		if _, err := o.Update(at, []Observation{obs("s", price, at)}); err != nil {
			t.Fatalf("step %d at %g: %v", i, price, err)
		}
		at = at.Add(5 * time.Minute)
		price *= 1.03 // 3% every five minutes, far apart enough not to trip
	}
	if broken, _ := o.Broken(at); broken {
		t.Error("a slow drift should not trip the breaker")
	}
	if price < 0.0090 {
		t.Fatalf("test did not actually move the price much: %g", price)
	}
}

// TestSpreadWidensWithVolatility: a calm hour and a violent one must not be
// priced the same.
func TestSpreadWidensWithVolatility(t *testing.T) {
	p := DefaultPolicy
	p.BaseSpread = 0.01
	p.VolatilityMultiple = 1.0
	p.MaxSpread = 0.10
	p.BreakerMove = 0.50 // high, so the moves below do not trip it

	calm, _ := New(p)
	at := now
	for i := 0; i < 5; i++ {
		r, err := calm.Update(at, []Observation{obs("s", 0.00300, at)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 4 && r.Spread != p.BaseSpread {
			t.Errorf("a flat market charged %.4f, want the base %.4f",
				r.Spread, p.BaseSpread)
		}
		at = at.Add(time.Minute)
	}

	rough, _ := New(p)
	at = now
	prices := []float64{0.00300, 0.00330, 0.00290, 0.00345, 0.00300}
	var lastSpread float64
	for _, price := range prices {
		r, err := rough.Update(at, []Observation{obs("s", price, at)})
		if err != nil {
			t.Fatal(err)
		}
		lastSpread = r.Spread
		at = at.Add(time.Minute)
	}
	if lastSpread <= p.BaseSpread {
		t.Errorf("a moving market charged %.4f, no more than the base %.4f",
			lastSpread, p.BaseSpread)
	}

	// And the cap holds.
	wild, _ := New(p)
	at = now
	for _, price := range []float64{0.00100, 0.00150, 0.00100, 0.00148} {
		r, err := wild.Update(at, []Observation{obs("s", price, at)})
		if err != nil {
			t.Fatal(err)
		}
		if r.Spread > p.MaxSpread {
			t.Fatalf("spread %.4f exceeded the cap %.4f", r.Spread, p.MaxSpread)
		}
		at = at.Add(time.Minute)
	}
}

// TestOperatorCanStopQuoting: a human who sees something the numbers do not
// must be able to stop the bridge without editing configuration.
func TestOperatorCanStopQuoting(t *testing.T) {
	o, _ := New(DefaultPolicy)
	o.Trip(now, "venue looks wrong")

	if _, err := o.Update(now, []Observation{obs("s", 0.00308, now)}); !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
	broken, why := o.Broken(now)
	if !broken || why != "venue looks wrong" {
		t.Errorf("got %v %q", broken, why)
	}
}

func TestRejectsBadInput(t *testing.T) {
	o, _ := New(DefaultPolicy)

	for _, bad := range []Observation{
		{Source: "zero", BTCPerBTCB2: 0, At: now},
		{Source: "negative", BTCPerBTCB2: -1, At: now},
		{Source: "nan", BTCPerBTCB2: math.NaN(), At: now},
		{Source: "inf", BTCPerBTCB2: math.Inf(1), At: now},
		{Source: "no time", BTCPerBTCB2: 0.003},
	} {
		if _, err := o.Update(now, []Observation{bad}); !errors.Is(err, ErrObservation) {
			t.Errorf("%s: want ErrObservation, got %v", bad.Source, err)
		}
	}

	// Bad policies are refused at construction.
	for _, p := range []Policy{
		{},
		{MaxAge: time.Minute, MaxDisagreement: 0, BreakerMove: 0.2, BreakerWindow: time.Minute, MaxSpread: 1},
		{MaxAge: time.Minute, MaxDisagreement: 0.05, BreakerMove: 0, BreakerWindow: time.Minute, MaxSpread: 1},
		{MaxAge: time.Minute, MaxDisagreement: 0.05, BreakerMove: 0.2, BreakerWindow: 0, MaxSpread: 1},
		// MaxSpread below BaseSpread is incoherent.
		{MaxAge: time.Minute, MaxDisagreement: 0.05, BreakerMove: 0.2, BreakerWindow: time.Minute, BaseSpread: 0.2, MaxSpread: 0.1},
	} {
		if _, err := New(p); !errors.Is(err, ErrPolicy) {
			t.Errorf("New(%+v) = %v, want ErrPolicy", p, err)
		}
	}
}

func TestDeriveRejectsBadInput(t *testing.T) {
	for _, c := range []struct{ btcb2, btc float64 }{
		{0, 77638}, {239, 0}, {-1, 77638}, {239, -1},
		{math.NaN(), 77638}, {239, math.Inf(1)},
	} {
		if _, err := Derive("s", c.btcb2, c.btc, now); !errors.Is(err, ErrObservation) {
			t.Errorf("Derive(%g, %g) = %v, want ErrObservation", c.btcb2,
				c.btc, err)
		}
	}
}

// TestHistoryIsBounded: the oracle runs for months and must not grow without
// limit.
func TestHistoryIsBounded(t *testing.T) {
	p := DefaultPolicy
	p.BreakerWindow = 10 * time.Minute
	o, _ := New(p)

	at := now
	for i := 0; i < 5000; i++ {
		if _, err := o.Update(at, []Observation{obs("s", 0.00300, at)}); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
	}
	// Ten minutes of one-second readings is about 600 entries, never 5000.
	if len(o.history) > 700 {
		t.Errorf("history grew to %d entries", len(o.history))
	}
}

// TestSingleSourceIsAllowedButCounted: the bridge can run on one source, and
// the caller needs to know the cross-check did not happen.
func TestSingleSourceIsAllowedButCounted(t *testing.T) {
	o, _ := New(DefaultPolicy)
	r, err := o.Update(now, []Observation{obs("only", 0.00308, now)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Sources != 1 {
		t.Errorf("Sources = %d, want 1", r.Sources)
	}
}

// TestThreeSourcesTakeTheMedian: an odd source out that is still inside the
// tolerance must not drag the answer as a mean would.
func TestThreeSourcesTakeTheMedian(t *testing.T) {
	o, _ := New(DefaultPolicy)
	r, err := o.Update(now, []Observation{
		obs("a", 0.00300, now),
		obs("b", 0.00305, now),
		obs("c", 0.00310, now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BTCPerBTCB2 != 0.00305 {
		t.Errorf("got %g, want the median 0.00305", r.BTCPerBTCB2)
	}
}

// TestZeroCooldownIsRefused covers the footgun found in review: Trip sets the
// breaker to expire after the cooldown, so a zero cooldown expires in the past
// and an operator's manual stop would silently do nothing at all.
func TestZeroCooldownIsRefused(t *testing.T) {
	p := DefaultPolicy
	p.Cooldown = 0
	if _, err := New(p); !errors.Is(err, ErrPolicy) {
		t.Fatalf("a zero cooldown must be refused, got %v", err)
	}

	// With a real cooldown, a manual trip actually stops quoting.
	p.Cooldown = time.Minute
	o, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	o.Trip(now, "by hand")
	if broken, _ := o.Broken(now); !broken {
		t.Fatal("a manual trip did not take effect")
	}
	if _, err := o.Update(now, []Observation{obs("s", 0.003, now)}); !errors.Is(err, ErrBroken) {
		t.Errorf("want ErrBroken, got %v", err)
	}
}
