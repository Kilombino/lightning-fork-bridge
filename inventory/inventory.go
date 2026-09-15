// Package inventory prices a swap by what the bridge is holding.
//
// It exists because the one number the design most wanted to know in advance —
// how much flow comes back the other way — turned out not to be knowable in
// advance. It is demand to be created rather than a fact to be measured, and a
// bridge that had to predict it before it could quote would never start.
//
// So it is not predicted. The spread is made a function of the bridge's
// measured position instead, and the flow ratio becomes something the bridge
// discovers by operating rather than something anyone asserts. If flow is
// one-way the outgoing side drains, the spread climbs to what one-way flow
// actually costs, and volume settles where the capital cycle puts it. If flow
// balances the side never drains, the spread stays near its floor, and nothing
// had to be re-decided for that to happen.
//
// The second half matters as much as the first. A swap in the replenishing
// direction is doing the bridge a favour it would otherwise pay a venue for,
// so it is quoted at a discount, and where a venue exists the discount is
// capped at exactly what a rebalance there would have cost: the bridge pays for
// rebalancing up to the price of doing it itself, and never more. Where no
// venue exists — the design's Stage 1 — that cap is lifted rather than set to
// zero, because with no alternative to flow a replenishing swap is worth more,
// not less.
package inventory

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	// ErrDrained is returned when the outgoing side is too low to quote the
	// draining direction at any price.
	ErrDrained = errors.New("outgoing inventory is below its floor")

	// ErrState is returned for a position that cannot be priced.
	ErrState = errors.New("inventory state is not usable")

	// ErrPolicy is returned for an incoherent policy.
	ErrPolicy = errors.New("inventory policy is not usable")

	// ErrStale is returned when the position is too old to price against.
	ErrStale = errors.New("inventory measurement is stale")
)

// Direction is which way a swap moves the bridge's position.
type Direction uint8

const (
	// Draining consumes outgoing inventory: the payer brings the incoming
	// asset and the bridge sends the outgoing one. This is the common
	// direction and the one that eventually forces a rebalance.
	Draining Direction = iota

	// Replenishing restores it: the payer brings the outgoing asset and the
	// bridge sends the incoming one. A mining payout arriving as BTC and
	// leaving as BTCB2 is this direction, which is the whole reason the
	// mining flow is worth having.
	Replenishing
)

var directionNames = map[Direction]string{
	Draining: "draining", Replenishing: "replenishing",
}

func (d Direction) String() string {
	if n, ok := directionNames[d]; ok {
		return n
	}

	return fmt.Sprintf("direction(%d)", uint8(d))
}

// State is what the bridge is holding, and when it last looked.
type State struct {
	// OutgoingMsat is what can still be paid out on the outgoing chain.
	// This is the binding constraint: the bridge can always accept more of
	// the incoming asset, and can never send more of the outgoing one than
	// it has.
	OutgoingMsat uint64

	// IncomingMsat is what has accumulated on the incoming side awaiting a
	// rebalance. It is not used for pricing — only the outgoing side binds —
	// but it is what an operator watches to decide when to cycle.
	IncomingMsat uint64

	// At is when this was measured.
	At time.Time
}

// Policy is how the spread responds.
type Policy struct {
	// TargetOutgoingMsat is the outgoing balance the bridge aims to hold. At
	// or above it there is no pressure and the spread sits at BaseSpread.
	TargetOutgoingMsat uint64

	// FloorOutgoingMsat is where the draining direction stops being quoted
	// at any price. It is not zero: a bridge that quotes until it is empty
	// has no reserve for the swaps already in flight, whose outgoing legs
	// have not been paid yet.
	FloorOutgoingMsat uint64

	// BaseSpread is charged at or above target, MaxSpread at the floor. The
	// measured venue numbers put the honest one-way price at 2.5-3%, so
	// MaxSpread below that quotes a direction the bridge cannot cover.
	BaseSpread float64
	MaxSpread  float64

	// MaxDiscount is the most that comes off BaseSpread in the replenishing
	// direction. Where a rebalance is possible it must not exceed
	// RebalanceCost: the bridge pays for rebalancing up to what doing it
	// itself would cost, and never more. It may never exceed BaseSpread,
	// which would quote a negative price.
	MaxDiscount float64

	// RebalanceCost is what it costs the bridge to restore its position by
	// any means other than flow, as a fraction. At an exchange that is the
	// bid/ask plus slippage; the measured BTCB2 bid/ask was 1.6%.
	//
	// **Zero means the bridge cannot rebalance at all**, which is the
	// design's Stage 1 configuration: no venue, no custody, flow is the only
	// mechanism. That is not the same as rebalancing being free, and the
	// difference matters in the direction people get wrong. With no venue a
	// replenishing swap is worth *more*, not less, because there is no
	// alternative to it — so the cap it would otherwise impose is lifted and
	// the binding limit becomes BaseSpread.
	RebalanceCost float64

	// Curve shapes how the response rises between target and floor. One is
	// linear. Above one holds the spread low until the balance is genuinely
	// short and then moves sharply; below one starts charging early.
	Curve float64

	// MaxAge is how old a position may be and still be priced against.
	MaxAge time.Duration
}

// DefaultPolicy is sized for the design's Stage 1 and the measured venue.
//
// The band is about $1,500 at an experiment budget of $2-3k, of which roughly a
// third is held on the outgoing side. MaxSpread is 3%, which is the honest
// price of one-way flow given a 1.6% bid/ask plus slippage and cycle time.
// BaseSpread is 1%, which covers routing and a thin margin when flow balances.
var DefaultPolicy = Policy{
	TargetOutgoingMsat: 50_000_000_000, // ~0.5 BTC-equivalent of working balance
	FloorOutgoingMsat:  5_000_000_000,  // a tenth of it, held back for swaps in flight
	BaseSpread:         0.01,
	MaxSpread:          0.03,
	MaxDiscount:        0.008,
	RebalanceCost:      0.016,
	Curve:              1.5,
	MaxAge:             2 * time.Minute,
}

// Valid reports whether the policy can be used.
func (p Policy) Valid() error {
	switch {
	case p.TargetOutgoingMsat == 0:
		return fmt.Errorf("%w: no target", ErrPolicy)

	case p.FloorOutgoingMsat >= p.TargetOutgoingMsat:
		return fmt.Errorf("%w: floor %d is not below target %d", ErrPolicy,
			p.FloorOutgoingMsat, p.TargetOutgoingMsat)

	case p.BaseSpread < 0 || p.MaxSpread < p.BaseSpread:
		return fmt.Errorf("%w: spreads %g..%g", ErrPolicy, p.BaseSpread,
			p.MaxSpread)

	case p.MaxSpread >= 1:
		return fmt.Errorf("%w: a spread of %g is not a quote", ErrPolicy,
			p.MaxSpread)

	case p.MaxDiscount < 0:
		return fmt.Errorf("%w: negative discount", ErrPolicy)

	case p.RebalanceCost < 0:
		return fmt.Errorf("%w: negative rebalance cost", ErrPolicy)

	case p.RebalanceCost > 0 && p.MaxDiscount > p.RebalanceCost:
		// Paying more for a rebalance than doing it yourself would cost is a
		// loss taken on purpose. The whole justification for the discount is
		// that it is cheaper than the alternative. Where there is no
		// alternative this does not apply, which is why it is conditional.
		return fmt.Errorf("%w: a discount of %.2f%% exceeds the %.2f%% a "+
			"rebalance costs, so the bridge would be paying over the odds to "+
			"be rebalanced", ErrPolicy, p.MaxDiscount*100,
			p.RebalanceCost*100)

	case p.RebalanceCost > 0 && p.MaxSpread < p.RebalanceCost:
		// At the floor the bridge is charging the most it ever will, and if
		// that is still under what restoring the position costs then every
		// swap in the draining direction loses money by construction —
		// which is the same failure the quote package refuses a too-wide
		// routing budget for, one level up.
		return fmt.Errorf("%w: a maximum spread of %.2f%% never covers the "+
			"%.2f%% a rebalance costs, so draining swaps lose at every "+
			"position", ErrPolicy, p.MaxSpread*100, p.RebalanceCost*100)

	case p.MaxDiscount > p.BaseSpread:
		// Otherwise the replenishing direction is quoted at a negative
		// spread, and a round trip pays the user to move money back and
		// forth.
		return fmt.Errorf("%w: a discount of %.2f%% exceeds the %.2f%% base "+
			"spread, which would quote a negative price", ErrPolicy,
			p.MaxDiscount*100, p.BaseSpread*100)

	case p.Curve <= 0:
		return fmt.Errorf("%w: curve %g", ErrPolicy, p.Curve)

	case p.MaxAge <= 0:
		return fmt.Errorf("%w: no maximum age", ErrPolicy)
	}

	return nil
}

// Reading is what the policy concluded.
type Reading struct {
	// Spread is the fraction to charge.
	Spread float64

	// Fill is how full the outgoing side is, between the floor and the
	// target: one is at or above target, zero is at the floor.
	Fill float64

	// Pressure is 1-Fill, the quantity the spread actually responds to.
	Pressure float64

	// Direction is the direction priced.
	Direction Direction
}

// fill is where the outgoing balance sits between floor and target.
func (p Policy) fill(s State) float64 {
	if s.OutgoingMsat >= p.TargetOutgoingMsat {
		return 1
	}
	if s.OutgoingMsat <= p.FloorOutgoingMsat {
		return 0
	}

	span := float64(p.TargetOutgoingMsat - p.FloorOutgoingMsat)

	return float64(s.OutgoingMsat-p.FloorOutgoingMsat) / span
}

// Spread prices a swap against the bridge's position.
//
// The draining direction is refused outright below the floor. That is not a
// pricing decision: the remaining balance is the reserve for swaps already in
// flight whose outgoing legs have not been paid, and quoting against it would
// sell the same money twice.
func (p Policy) Spread(s State, dir Direction, now time.Time) (Reading, error) {
	if err := p.Valid(); err != nil {
		return Reading{}, err
	}
	if s.At.IsZero() {
		return Reading{}, fmt.Errorf("%w: the position carries no "+
			"measurement time", ErrState)
	}
	if age := now.Sub(s.At); age > p.MaxAge {
		return Reading{}, fmt.Errorf("%w: measured %v ago, limit %v",
			ErrStale, age.Round(time.Second), p.MaxAge)
	}
	if s.At.After(now.Add(p.MaxAge)) {
		return Reading{}, fmt.Errorf("%w: measured %v in the future",
			ErrState, s.At.Sub(now).Round(time.Second))
	}

	f := p.fill(s)
	pressure := math.Pow(1-f, p.Curve)
	out := Reading{Fill: f, Pressure: pressure, Direction: dir}

	switch dir {
	case Draining:
		if s.OutgoingMsat <= p.FloorOutgoingMsat {
			return Reading{}, fmt.Errorf("%w: %d msat against a floor of %d; "+
				"what is left is the reserve for swaps already in flight",
				ErrDrained, s.OutgoingMsat, p.FloorOutgoingMsat)
		}
		out.Spread = p.BaseSpread + (p.MaxSpread-p.BaseSpread)*pressure

	case Replenishing:
		// The emptier the bridge is, the more a swap in this direction is
		// worth to it, and the more of the base spread it gives back. This
		// is what makes the mining flow valuable without anyone having to
		// know how large it will be: it simply gets cheaper exactly when it
		// is most needed.
		out.Spread = p.BaseSpread - p.MaxDiscount*pressure
		if out.Spread < 0 {
			// Unreachable while Valid holds, since MaxDiscount cannot
			// exceed BaseSpread. Kept because a negative spread pays people
			// to round-trip money through the bridge.
			out.Spread = 0
		}

	default:
		return Reading{}, fmt.Errorf("%w: unknown %v", ErrState, dir)
	}

	return out, nil
}

// Headroom is how much more of the outgoing asset can be committed before the
// floor, which is the real cap on a single swap.
func (p Policy) Headroom(s State) uint64 {
	if s.OutgoingMsat <= p.FloorOutgoingMsat {
		return 0
	}

	return s.OutgoingMsat - p.FloorOutgoingMsat
}

// RoundTrip is what a user pays to swap out and back at the current position.
//
// It must stay positive. If it ever went negative the bridge would be paying
// people to move money in a circle, which is not a flow it wants more of.
func (p Policy) RoundTrip(s State, now time.Time) (float64, error) {
	out, err := p.Spread(s, Draining, now)
	if err != nil {
		return 0, err
	}
	back, err := p.Spread(s, Replenishing, now)
	if err != nil {
		return 0, err
	}

	return out.Spread + back.Spread, nil
}
