// Package confirm decides how many confirmations a value at risk needs.
//
// A confirmation is worth what it costs to produce, and on the BLAKE2b chain
// that is not much. Both chains pay the same 3.125 coin subsidy, having the
// same supply schedule, but a BLAKE2b coin is worth a few hundred dollars
// against Bitcoin's tens of thousands, so a block there costs roughly 1/325th
// as much to mine. Six confirmations mean something quite different on each.
//
// The measure used here is the forgone block subsidy: roughly what a rational
// miner gives up by abandoning the honest chain to rewrite N blocks. It is a
// lower bound on the cost of an attack, since it assumes hardware that is
// already mining honestly rather than rented or repurposed, and this chain has
// shown that large hashrate can arrive within days. The safety multiple is
// there for that gap, and the numbers should be revisited as hashrate
// concentration becomes visible.
//
// Because the gap between the chains is a price ratio rather than a work
// ratio, it moves. Economics must be read live rather than compiled in.
package confirm

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrEconomics is returned when the subsidy or price is missing or
	// nonsensical, which must stop a decision rather than default to
	// something permissive.
	ErrEconomics = errors.New("block economics are not usable")

	// ErrValue is returned for a value at risk that is negative or not a
	// number.
	ErrValue = errors.New("value at risk is not usable")

	// ErrPolicy is returned for an incoherent policy.
	ErrPolicy = errors.New("confirmation policy is not usable")

	// ErrTooDeep is returned when the depth required exceeds the ceiling.
	// The value should be split, or moved over Lightning instead of on
	// chain; waiting is no longer a practical answer.
	ErrTooDeep = errors.New("required depth exceeds the policy ceiling")
)

// Economics is what one block on a chain is worth to whoever produces it.
//
// Price and the value at risk passed to Depth must be in the same unit. Which
// unit does not matter, as long as it is the same one.
type Economics struct {
	// Subsidy is the block reward in coins. Fees are ignored, which is
	// conservative: counting them would make an attack look more expensive.
	Subsidy float64

	// Price is what one coin is worth.
	Price float64
}

// BlockValue is what abandoning one block costs.
func (e Economics) BlockValue() float64 {
	return e.Subsidy * e.Price
}

// Valid reports whether the economics can be used.
func (e Economics) Valid() bool {
	v := e.BlockValue()

	return e.Subsidy > 0 && e.Price > 0 && !math.IsInf(v, 0) && v > 0
}

// Policy is the operator's confirmation settings.
type Policy struct {
	// Safety is the multiple of break-even to require. At 1 an attacker
	// breaks even; the point of a larger number is that break-even is a
	// lower bound on their cost, not an estimate of it.
	Safety float64

	// Floor is the fewest confirmations ever accepted, whatever the
	// arithmetic says, so that a trivial value is not waved through on one
	// block.
	Floor uint32

	// Ceiling is the most that will be waited for. Beyond it the answer is
	// to stop using the chain for that value rather than to wait longer.
	Ceiling uint32
}

// DefaultPolicy matches the design document: three times break-even, never
// fewer than Bitcoin's customary six, and never more than a day of blocks at
// the ten minute target.
var DefaultPolicy = Policy{
	Safety:  3,
	Floor:   6,
	Ceiling: 144,
}

// Valid reports whether the policy is usable.
func (p Policy) Valid() bool {
	return p.Safety > 0 && !math.IsInf(p.Safety, 0) && !math.IsNaN(p.Safety) &&
		p.Floor > 0 && p.Ceiling >= p.Floor
}

// Depth returns the confirmations required before a value may be treated as
// settled.
//
//	N = ceil(safety * value / block value), floored and capped by the policy.
func Depth(valueAtRisk float64, e Economics, p Policy) (uint32, error) {
	if math.IsNaN(valueAtRisk) || math.IsInf(valueAtRisk, 0) ||
		valueAtRisk < 0 {

		return 0, fmt.Errorf("%w: %v", ErrValue, valueAtRisk)
	}
	if !e.Valid() {
		return 0, fmt.Errorf("%w: subsidy %v, price %v", ErrEconomics,
			e.Subsidy, e.Price)
	}
	if !p.Valid() {
		return 0, fmt.Errorf("%w: %+v", ErrPolicy, p)
	}

	exact := p.Safety * valueAtRisk / e.BlockValue()
	if math.IsInf(exact, 0) || math.IsNaN(exact) {
		return 0, fmt.Errorf("%w: %v at risk against a block worth %v",
			ErrValue, valueAtRisk, e.BlockValue())
	}

	// Compare before converting: a float larger than uint32 wraps silently.
	if exact > float64(p.Ceiling) {
		return 0, fmt.Errorf("%w: need %.0f confirmations, ceiling %d",
			ErrTooDeep, math.Ceil(exact), p.Ceiling)
	}

	need := uint32(math.Ceil(exact))
	if need < p.Floor {
		need = p.Floor
	}

	return need, nil
}

// MaxValue is the largest value the policy will let this chain settle on
// chain, which is the ceiling's worth of forgone subsidy. Above it, Depth
// returns ErrTooDeep and the value has to be split or kept off chain.
func MaxValue(e Economics, p Policy) (float64, error) {
	if !e.Valid() {
		return 0, fmt.Errorf("%w: subsidy %v, price %v", ErrEconomics,
			e.Subsidy, e.Price)
	}
	if !p.Valid() {
		return 0, fmt.Errorf("%w: %+v", ErrPolicy, p)
	}

	return float64(p.Ceiling) * e.BlockValue() / p.Safety, nil
}
