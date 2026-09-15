package inventory

import (
	"errors"
	"math"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func at(outgoing uint64) State {
	return State{OutgoingMsat: outgoing, At: now}
}

// TestTheFlowRatioIsDiscoveredNotPredicted is the property this package was
// written for.
//
// Nobody knows how much flow comes back the other way. What this proves is
// that nobody needs to: the same policy, unchanged, prices one-way flow at
// what one-way flow costs and balanced flow near its floor, purely from what
// the bridge is holding.
func TestTheFlowRatioIsDiscoveredNotPredicted(t *testing.T) {
	p := DefaultPolicy

	// Balanced flow: the outgoing side never drains, because swaps in the
	// replenishing direction keep refilling it.
	balanced, err := p.Spread(at(p.TargetOutgoingMsat), Draining, now)
	if err != nil {
		t.Fatal(err)
	}
	if balanced.Spread != p.BaseSpread {
		t.Errorf("balanced flow priced at %.2f%%, want the base %.2f%%",
			balanced.Spread*100, p.BaseSpread*100)
	}

	// One-way flow: nothing comes back, the side drains, and the price
	// climbs to what the venue actually costs to cover.
	nearFloor := p.FloorOutgoingMsat + 1
	oneWay, err := p.Spread(at(nearFloor), Draining, now)
	if err != nil {
		t.Fatal(err)
	}
	if oneWay.Spread <= balanced.Spread {
		t.Fatalf("one-way flow priced at %.2f%%, no more than balanced %.2f%%",
			oneWay.Spread*100, balanced.Spread*100)
	}
	if oneWay.Spread < p.RebalanceCost {
		t.Errorf("one-way flow priced at %.2f%%, under the %.2f%% a venue "+
			"rebalance costs, so every such swap loses",
			oneWay.Spread*100, p.RebalanceCost*100)
	}

	t.Logf("balanced %.2f%%, one-way %.2f%%, with nobody having predicted "+
		"which it would be", balanced.Spread*100, oneWay.Spread*100)
}

// TestDrainingGetsDearerAsTheBridgeEmpties, monotonically, with no flat spots
// where a large swing in position leaves the price unchanged.
func TestDrainingGetsDearerAsTheBridgeEmpties(t *testing.T) {
	p := DefaultPolicy
	span := p.TargetOutgoingMsat - p.FloorOutgoingMsat

	var last float64 = -1
	for i := 20; i >= 1; i-- {
		bal := p.FloorOutgoingMsat + span*uint64(i)/20
		got, err := p.Spread(at(bal), Draining, now)
		if err != nil {
			t.Fatalf("%d/20: %v", i, err)
		}
		if last >= 0 && got.Spread < last {
			t.Errorf("%d/20 full: spread fell from %.4f to %.4f as the "+
				"bridge emptied", i, last, got.Spread)
		}
		last = got.Spread
	}

	// The endpoints are exactly the policy's, not merely near it.
	full, _ := p.Spread(at(p.TargetOutgoingMsat), Draining, now)
	if full.Spread != p.BaseSpread {
		t.Errorf("at target the spread is %g, want %g", full.Spread,
			p.BaseSpread)
	}
	empty, _ := p.Spread(at(p.FloorOutgoingMsat+1), Draining, now)
	if math.Abs(empty.Spread-p.MaxSpread) > 1e-6 {
		t.Errorf("at the floor the spread is %g, want %g", empty.Spread,
			p.MaxSpread)
	}

	// Above target there is no pressure at all, however much is held.
	for _, bal := range []uint64{
		p.TargetOutgoingMsat, p.TargetOutgoingMsat * 2,
		p.TargetOutgoingMsat * 100,
	} {
		got, err := p.Spread(at(bal), Draining, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Spread != p.BaseSpread || got.Fill != 1 {
			t.Errorf("%d msat: spread %g fill %g", bal, got.Spread, got.Fill)
		}
	}
}

// TestReplenishingGetsCheaperAsTheBridgeEmpties. A swap in this direction is
// doing the bridge a favour it would otherwise pay a venue for, and it is
// worth most exactly when the bridge is shortest.
func TestReplenishingGetsCheaperAsTheBridgeEmpties(t *testing.T) {
	p := DefaultPolicy
	span := p.TargetOutgoingMsat - p.FloorOutgoingMsat

	full, err := p.Spread(at(p.TargetOutgoingMsat), Replenishing, now)
	if err != nil {
		t.Fatal(err)
	}
	if full.Spread != p.BaseSpread {
		t.Errorf("at target replenishing costs %g, want the base %g",
			full.Spread, p.BaseSpread)
	}

	var last = math.Inf(1)
	for i := 20; i >= 0; i-- {
		bal := p.FloorOutgoingMsat + span*uint64(i)/20
		got, err := p.Spread(at(bal), Replenishing, now)
		if err != nil {
			t.Fatalf("%d/20: %v", i, err)
		}
		if got.Spread > last {
			t.Errorf("%d/20 full: replenishing got dearer, %.4f then %.4f",
				i, last, got.Spread)
		}
		if got.Spread < 0 {
			t.Errorf("%d/20: negative spread %g pays people to round-trip",
				i, got.Spread)
		}
		last = got.Spread
	}

	// At the floor it is the base less the whole discount.
	if want := p.BaseSpread - p.MaxDiscount; math.Abs(last-want) > 1e-9 {
		t.Errorf("at the floor replenishing costs %g, want %g", last, want)
	}
	t.Logf("replenishing costs %.2f%% at target and %.2f%% at the floor",
		full.Spread*100, last*100)
}

// TestTheBridgeIsStillQuotedAtTheFloorInTheDirectionThatHelps. Refusing
// everything when short would refuse exactly the swaps that fix the problem.
func TestTheBridgeIsStillQuotedAtTheFloorInTheDirectionThatHelps(t *testing.T) {
	p := DefaultPolicy

	for _, bal := range []uint64{p.FloorOutgoingMsat, p.FloorOutgoingMsat / 2, 0} {
		if _, err := p.Spread(at(bal), Draining, now); !errors.Is(err,
			ErrDrained) {

			t.Errorf("%d msat: draining should be refused, got %v", bal, err)
		}

		got, err := p.Spread(at(bal), Replenishing, now)
		if err != nil {
			t.Fatalf("%d msat: replenishing was refused too, which would "+
				"refuse the swaps that fix the shortage: %v", bal, err)
		}
		if want := p.BaseSpread - p.MaxDiscount; math.Abs(got.Spread-want) >
			1e-9 {

			t.Errorf("%d msat: replenishing at %g, want %g", bal, got.Spread,
				want)
		}
	}
}

// TestARoundTripAlwaysCosts. If it ever went free the bridge would be paying
// people to move money in a circle, which is not a flow it wants more of, and
// is the obvious way to drain a subsidy.
func TestARoundTripAlwaysCosts(t *testing.T) {
	p := DefaultPolicy
	span := p.TargetOutgoingMsat - p.FloorOutgoingMsat

	for i := 1; i <= 40; i++ {
		bal := p.FloorOutgoingMsat + span*uint64(i)/40
		cost, err := p.RoundTrip(at(bal), now)
		if err != nil {
			t.Fatalf("%d msat: %v", bal, err)
		}
		if cost <= 0 {
			t.Errorf("%d msat: a round trip costs %g", bal, cost)
		}
		if cost < p.BaseSpread {
			t.Errorf("%d msat: a round trip costs %g, under one base spread",
				bal, cost)
		}
	}
}

// TestTheDiscountNeverExceedsDoingItYourself. The entire justification for
// paying someone to rebalance the bridge is that it is cheaper than the venue.
func TestTheDiscountNeverExceedsDoingItYourself(t *testing.T) {
	p := DefaultPolicy
	if p.MaxDiscount > p.RebalanceCost {
		t.Fatalf("the default policy pays %.2f%% for a rebalance that costs "+
			"%.2f%% to do directly", p.MaxDiscount*100, p.RebalanceCost*100)
	}

	bad := p
	bad.MaxDiscount = p.RebalanceCost + 0.001
	if err := bad.Valid(); !errors.Is(err, ErrPolicy) {
		t.Errorf("a discount over the venue cost was accepted: %v", err)
	}

	worse := p
	worse.MaxDiscount = p.BaseSpread + 0.001
	worse.RebalanceCost = 1
	if err := worse.Valid(); !errors.Is(err, ErrPolicy) {
		t.Errorf("a discount over the base spread was accepted: %v", err)
	}
}

// TestOneWayFlowIsNeverQuotedUnderWhatItCosts. The measured venue bid/ask is
// 1.6% and slippage is on top, so a MaxSpread under that quotes a direction
// the bridge cannot cover.
func TestOneWayFlowIsNeverQuotedUnderWhatItCosts(t *testing.T) {
	p := DefaultPolicy
	if p.MaxSpread < p.RebalanceCost {
		t.Errorf("the default policy tops out at %.2f%% against a venue cost "+
			"of %.2f%%", p.MaxSpread*100, p.RebalanceCost*100)
	}

	// And the curve must actually reach it, not merely approach it.
	empty, err := p.Spread(at(p.FloorOutgoingMsat+1), Draining, now)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Spread < p.RebalanceCost {
		t.Errorf("even at the floor the spread is %.2f%%, under the %.2f%% "+
			"cost", empty.Spread*100, p.RebalanceCost*100)
	}
}

// TestTheCurveShapesTheResponseWithoutChangingItsEnds.
func TestTheCurveShapesTheResponseWithoutChangingItsEnds(t *testing.T) {
	half := DefaultPolicy.FloorOutgoingMsat +
		(DefaultPolicy.TargetOutgoingMsat-DefaultPolicy.FloorOutgoingMsat)/2

	var mid []float64
	for _, curve := range []float64{0.5, 1, 2} {
		p := DefaultPolicy
		p.Curve = curve

		full, _ := p.Spread(at(p.TargetOutgoingMsat), Draining, now)
		empty, _ := p.Spread(at(p.FloorOutgoingMsat+1), Draining, now)
		if full.Spread != p.BaseSpread {
			t.Errorf("curve %g moved the full end to %g", curve, full.Spread)
		}
		if math.Abs(empty.Spread-p.MaxSpread) > 1e-6 {
			t.Errorf("curve %g moved the empty end to %g", curve, empty.Spread)
		}

		got, _ := p.Spread(at(half), Draining, now)
		mid = append(mid, got.Spread)
	}

	// A lower curve charges earlier, so at the midpoint it must be dearer.
	if !(mid[0] > mid[1] && mid[1] > mid[2]) {
		t.Errorf("at half full the curves gave %v, want strictly decreasing "+
			"as the curve rises", mid)
	}
}

// TestAStalePositionPricesNothing. Quoting against a balance measured minutes
// ago can sell money that has already left on another swap.
func TestAStalePositionPricesNothing(t *testing.T) {
	p := DefaultPolicy

	for _, c := range []struct {
		name  string
		state State
		want  error
	}{
		{"no measurement time", State{OutgoingMsat: p.TargetOutgoingMsat},
			ErrState},
		{"measured too long ago", State{
			OutgoingMsat: p.TargetOutgoingMsat,
			At:           now.Add(-p.MaxAge - time.Second),
		}, ErrStale},
		{"measured in the future", State{
			OutgoingMsat: p.TargetOutgoingMsat,
			At:           now.Add(2 * p.MaxAge),
		}, ErrState},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, dir := range []Direction{Draining, Replenishing} {
				if _, err := p.Spread(c.state, dir, now); !errors.Is(err,
					c.want) {

					t.Errorf("%v: want %v, got %v", dir, c.want, err)
				}
			}
		})
	}

	// Just inside the limit is fine, so the boundary is not off by one.
	fresh := State{OutgoingMsat: p.TargetOutgoingMsat, At: now.Add(-p.MaxAge)}
	if _, err := p.Spread(fresh, Draining, now); err != nil {
		t.Errorf("a position exactly at the age limit was refused: %v", err)
	}
}

func TestPolicyRefusesWhatCannotWork(t *testing.T) {
	good := DefaultPolicy

	for _, c := range []struct {
		name string
		with func(p *Policy)
	}{
		{"no target", func(p *Policy) { p.TargetOutgoingMsat = 0 }},
		{"a floor at the target", func(p *Policy) {
			p.FloorOutgoingMsat = p.TargetOutgoingMsat
		}},
		{"a floor above the target", func(p *Policy) {
			p.FloorOutgoingMsat = p.TargetOutgoingMsat + 1
		}},
		{"a negative base spread", func(p *Policy) { p.BaseSpread = -0.01 }},
		{"a maximum under the base", func(p *Policy) { p.MaxSpread = 0.001 }},
		{"a spread that is not a quote", func(p *Policy) { p.MaxSpread = 1.5 }},
		{"a negative discount", func(p *Policy) { p.MaxDiscount = -0.01 }},
		{"a negative venue cost", func(p *Policy) { p.RebalanceCost = -0.01 }},
		{"no curve", func(p *Policy) { p.Curve = 0 }},
		{"a negative curve", func(p *Policy) { p.Curve = -1 }},
		{"no maximum age", func(p *Policy) { p.MaxAge = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.with(&p)
			if err := p.Valid(); !errors.Is(err, ErrPolicy) {
				t.Errorf("want ErrPolicy, got %v", err)
			}
			// And nothing is priced against an unusable policy.
			if _, err := p.Spread(at(p.TargetOutgoingMsat), Draining,
				now); !errors.Is(err, ErrPolicy) {

				t.Errorf("it priced anyway: %v", err)
			}
		})
	}

	if err := good.Valid(); err != nil {
		t.Errorf("the default policy is not valid: %v", err)
	}
}

func TestHeadroomIsWhatCanStillBeCommitted(t *testing.T) {
	p := DefaultPolicy

	for _, c := range []struct {
		balance uint64
		want    uint64
	}{
		{p.TargetOutgoingMsat, p.TargetOutgoingMsat - p.FloorOutgoingMsat},
		{p.FloorOutgoingMsat, 0},
		{p.FloorOutgoingMsat - 1, 0},
		{0, 0},
		{p.FloorOutgoingMsat + 1000, 1000},
	} {
		if got := p.Headroom(at(c.balance)); got != c.want {
			t.Errorf("%d msat: headroom %d, want %d", c.balance, got, c.want)
		}
	}
}

func TestUnknownDirectionIsRefused(t *testing.T) {
	if _, err := DefaultPolicy.Spread(at(DefaultPolicy.TargetOutgoingMsat),
		Direction(9), now); !errors.Is(err, ErrState) {

		t.Errorf("want ErrState, got %v", err)
	}
	if got := Direction(9).String(); got != "direction(9)" {
		t.Errorf("unknown direction prints %q", got)
	}
}

// FuzzSpread: whatever the position and policy, a usable answer is a spread
// between zero and one, and a round trip never pays the user.
func FuzzSpread(f *testing.F) {
	f.Add(uint64(50_000_000_000), uint64(5_000_000_000),
		uint64(50_000_000_000), 0.01, 0.03, 0.008, 1.5)

	f.Fuzz(func(t *testing.T, balance, floor, target uint64,
		base, max, discount, curve float64) {

		p := Policy{
			TargetOutgoingMsat: target, FloorOutgoingMsat: floor,
			BaseSpread: base, MaxSpread: max, MaxDiscount: discount,
			RebalanceCost: 1, Curve: curve, MaxAge: time.Minute,
		}
		if p.Valid() != nil {
			t.Skip()
		}

		s := at(balance)
		out, outErr := p.Spread(s, Draining, now)
		back, backErr := p.Spread(s, Replenishing, now)

		for _, r := range []struct {
			r   Reading
			err error
		}{{out, outErr}, {back, backErr}} {
			if r.err != nil {
				continue
			}
			if math.IsNaN(r.r.Spread) || math.IsInf(r.r.Spread, 0) {
				t.Fatalf("spread %g from %+v", r.r.Spread, p)
			}
			if r.r.Spread < 0 || r.r.Spread >= 1 {
				t.Fatalf("spread %g is not a quote", r.r.Spread)
			}
			if r.r.Fill < 0 || r.r.Fill > 1 {
				t.Fatalf("fill %g", r.r.Fill)
			}
		}

		if outErr == nil && backErr == nil {
			if trip := out.Spread + back.Spread; trip <= 0 {
				t.Fatalf("a round trip costs %g at %+v", trip, p)
			}
		}
	})
}

// TestNoVenueIsNotFreeRebalancing is the case Stage 1 actually runs in: no
// exchange, no custody, flow is the only way the position moves.
//
// A zero rebalance cost must not be read as "rebalancing is free", which would
// cap the discount at nothing — exactly backwards. With no alternative to
// flow, a replenishing swap is worth more, not less, so the cap is lifted and
// what binds is the base spread.
func TestNoVenueIsNotFreeRebalancing(t *testing.T) {
	p := DefaultPolicy
	p.RebalanceCost = 0
	p.MaxDiscount = p.BaseSpread // the whole spread, given away

	if err := p.Valid(); err != nil {
		t.Fatalf("a no-venue policy was refused: %v", err)
	}

	// And a maximum spread under the old venue cost is now fine, because
	// there is no venue cost to cover.
	cheap := p
	cheap.MaxSpread = 0.012
	if err := cheap.Valid(); err != nil {
		t.Errorf("a no-venue policy was held to a venue's costs: %v", err)
	}

	// At the floor, replenishing is free: the bridge gives away its entire
	// margin for the only thing that can restore its position.
	got, err := p.Spread(at(p.FloorOutgoingMsat), Replenishing, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spread != 0 {
		t.Errorf("at the floor with no venue, replenishing costs %g", got.Spread)
	}

	// A round trip still is not free, because the draining half is not.
	trip, err := p.RoundTrip(at(p.FloorOutgoingMsat+1), now)
	if err != nil {
		t.Fatal(err)
	}
	if trip <= 0 {
		t.Errorf("a round trip costs %g", trip)
	}
	t.Logf("with no venue: replenishing free at the floor, round trip still "+
		"%.2f%%", trip*100)

	// The discount may still never exceed the base spread, venue or no.
	bad := p
	bad.MaxDiscount = p.BaseSpread + 0.001
	if err := bad.Valid(); !errors.Is(err, ErrPolicy) {
		t.Errorf("a negative price was accepted with no venue: %v", err)
	}
}

// TestAMaxSpreadThatNeverCoversARebalance is refused. At the floor the bridge
// charges the most it ever will; if that still does not cover restoring the
// position, every draining swap loses by construction.
func TestAMaxSpreadThatNeverCoversARebalance(t *testing.T) {
	p := DefaultPolicy
	p.MaxSpread = p.RebalanceCost - 0.001
	p.BaseSpread = 0.001
	p.MaxDiscount = 0.001

	if err := p.Valid(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("want ErrPolicy, got %v", err)
	}
}
