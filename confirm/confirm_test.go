package confirm

import (
	"errors"
	"math"
	"testing"
)

// Measured on 2026-09-14. BTCB2 at $239.19 a coin against Bitcoin at $77,638,
// both paying 3.125 a block.
var (
	blake = Economics{Subsidy: 3.125, Price: 239.19}
	btc   = Economics{Subsidy: 3.125, Price: 77638}
)

// TestTheRatioThisPackageExistsFor records the gap that makes block counts
// useless across the two chains.
func TestTheRatioThisPackageExistsFor(t *testing.T) {
	ratio := btc.BlockValue() / blake.BlockValue()
	if ratio < 300 || ratio > 350 {
		t.Fatalf("a Bitcoin block is worth %.0fx a BLAKE2b one; the design "+
			"document says about 325x, so one of them is stale", ratio)
	}

	// Six confirmations, the Bitcoin convention, bought on each chain.
	sixBlake := 6 * blake.BlockValue() / DefaultPolicy.Safety
	sixBtc := 6 * btc.BlockValue() / DefaultPolicy.Safety
	if sixBlake < 1400 || sixBlake > 1600 {
		t.Errorf("six BLAKE2b confirmations cover $%.0f, want about $1,495",
			sixBlake)
	}
	if sixBtc < 450000 || sixBtc > 500000 {
		t.Errorf("six Bitcoin confirmations cover $%.0f, want about $485,000",
			sixBtc)
	}
}

// TestDocumentedTable pins the design document's worked figures.
func TestDocumentedTable(t *testing.T) {
	p := Policy{Safety: 3, Floor: 6, Ceiling: 1000}

	cases := []struct {
		value float64
		want  uint32
	}{
		{0, 6},     // the floor covers a trivial amount
		{150, 6},   // a Stage 1 per-swap cap
		{1494, 6},  // still inside the floor
		{1500, 7},  // just past it
		{3000, 13}, // the document says 12; ceil of 12.04
		{10000, 41},
		{36000, 145},
	}
	for _, c := range cases {
		got, err := Depth(c.value, blake, p)
		if err != nil {
			t.Fatalf("$%.0f: %v", c.value, err)
		}
		if got != c.want {
			t.Errorf("$%.0f needs %d confirmations, want %d", c.value,
				got, c.want)
		}
	}
}

// TestStage1NeedsNoCode checks the claim that Stage 1's values sit inside the
// floor, which is why the node's existing behaviour is already adequate.
func TestStage1NeedsNoCode(t *testing.T) {
	p := DefaultPolicy
	for _, v := range []float64{150, 170, 1000, 1400} {
		got, err := Depth(v, blake, p)
		if err != nil {
			t.Fatalf("$%.0f: %v", v, err)
		}
		if got != p.Floor {
			t.Errorf("$%.0f needs %d confirmations, want the floor of %d",
				v, got, p.Floor)
		}
	}
}

// TestExactBoundary uses round numbers so the boundary is not a question of
// floating point. A block worth 100 at safety 3 covers 200 in six blocks.
func TestExactBoundary(t *testing.T) {
	e := Economics{Subsidy: 1, Price: 100}
	p := Policy{Safety: 3, Floor: 1, Ceiling: 1000}

	if got, err := Depth(200, e, p); err != nil || got != 6 {
		t.Errorf("200 exactly: got %d (%v), want 6", got, err)
	}
	if got, err := Depth(201, e, p); err != nil || got != 7 {
		t.Errorf("just over: got %d (%v), want 7", got, err)
	}
	if got, err := Depth(199, e, p); err != nil || got != 6 {
		t.Errorf("just under: got %d (%v), want 6", got, err)
	}
}

// TestPriceMovesTheAnswer is the reason economics are read live. The same
// value needs more confirmations when the coin is worth less.
func TestPriceMovesTheAnswer(t *testing.T) {
	p := Policy{Safety: 3, Floor: 1, Ceiling: 100000}

	full, err := Depth(10000, blake, p)
	if err != nil {
		t.Fatal(err)
	}
	halved := blake
	halved.Price = blake.Price / 2
	after, err := Depth(10000, halved, p)
	if err != nil {
		t.Fatal(err)
	}
	if after != full*2 && after != full*2-1 && after != full*2+1 {
		t.Errorf("halving the price gave %d confirmations against %d; "+
			"expected roughly double", after, full)
	}
	if after <= full {
		t.Errorf("a cheaper coin must need more confirmations, got %d vs %d",
			after, full)
	}
}

// TestMonotonic: nothing may make a larger value need fewer confirmations.
func TestMonotonic(t *testing.T) {
	p := Policy{Safety: 3, Floor: 1, Ceiling: 1 << 20}

	var prev uint32
	for v := 0.0; v < 50000; v += 137 {
		got, err := Depth(v, blake, p)
		if err != nil {
			t.Fatalf("$%.0f: %v", v, err)
		}
		if got < prev {
			t.Fatalf("$%.0f needs %d, less than the %d a smaller value "+
				"needed", v, got, prev)
		}
		prev = got
	}
}

func TestCeiling(t *testing.T) {
	p := DefaultPolicy // ceiling 144

	max, err := MaxValue(blake, p)
	if err != nil {
		t.Fatal(err)
	}
	if max < 35000 || max > 36500 {
		t.Errorf("ceiling covers $%.0f, want about $35,900", max)
	}

	// Just inside the ceiling works, just outside is refused rather than
	// quietly truncated to the ceiling.
	if _, err := Depth(max-1, blake, p); err != nil {
		t.Errorf("just inside the ceiling: %v", err)
	}
	if _, err := Depth(max*1.01, blake, p); !errors.Is(err, ErrTooDeep) {
		t.Errorf("past the ceiling: got %v, want ErrTooDeep", err)
	}

	// A huge value must not wrap a uint32.
	if _, err := Depth(1e18, blake, p); !errors.Is(err, ErrTooDeep) {
		t.Errorf("an enormous value: got %v, want ErrTooDeep", err)
	}
}

func TestRejectsBadInput(t *testing.T) {
	p := DefaultPolicy

	cases := []struct {
		name  string
		value float64
		e     Economics
		p     Policy
		want  error
	}{
		{"negative value", -1, blake, p, ErrValue},
		{"NaN value", math.NaN(), blake, p, ErrValue},
		{"infinite value", math.Inf(1), blake, p, ErrValue},
		{"zero price", 100, Economics{Subsidy: 3.125}, p, ErrEconomics},
		{"zero subsidy", 100, Economics{Price: 100}, p, ErrEconomics},
		{"negative price", 100, Economics{Subsidy: 3.125, Price: -1}, p, ErrEconomics},
		{"NaN price", 100, Economics{Subsidy: 3.125, Price: math.NaN()}, p, ErrEconomics},
		{"infinite price", 100, Economics{Subsidy: 1, Price: math.Inf(1)}, p, ErrEconomics},
		{"zero safety", 100, blake, Policy{Floor: 6, Ceiling: 144}, ErrPolicy},
		{"NaN safety", 100, blake, Policy{Safety: math.NaN(), Floor: 6, Ceiling: 144}, ErrPolicy},
		{"zero floor", 100, blake, Policy{Safety: 3, Ceiling: 144}, ErrPolicy},
		{"ceiling below floor", 100, blake, Policy{Safety: 3, Floor: 10, Ceiling: 5}, ErrPolicy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Depth(c.value, c.e, c.p); !errors.Is(err, c.want) {
				t.Errorf("Depth: got %v, want %v", err, c.want)
			}
		})
	}

	// MaxValue rejects the same bad economics and policy.
	if _, err := MaxValue(Economics{}, p); !errors.Is(err, ErrEconomics) {
		t.Errorf("MaxValue with no economics: %v", err)
	}
	if _, err := MaxValue(blake, Policy{}); !errors.Is(err, ErrPolicy) {
		t.Errorf("MaxValue with no policy: %v", err)
	}
}

// FuzzDepth looks for panics, silent wraps and floor or ceiling violations.
func FuzzDepth(f *testing.F) {
	f.Add(1500.0, 3.125, 239.19, 3.0, uint32(6), uint32(144))
	f.Add(0.0, 1.0, 1.0, 1.0, uint32(1), uint32(1))

	f.Fuzz(func(t *testing.T, value, subsidy, price, safety float64,
		floor, ceiling uint32) {

		e := Economics{Subsidy: subsidy, Price: price}
		p := Policy{Safety: safety, Floor: floor, Ceiling: ceiling}

		got, err := Depth(value, e, p)
		if err != nil {
			if got != 0 {
				t.Fatalf("an error must return zero, got %d", got)
			}

			return
		}
		if got < p.Floor {
			t.Fatalf("%d confirmations is under the floor of %d", got, p.Floor)
		}
		if got > p.Ceiling {
			t.Fatalf("%d confirmations is over the ceiling of %d", got,
				p.Ceiling)
		}
		// Whatever was returned must actually cover the value.
		if float64(got)*e.BlockValue()/p.Safety < value*0.999999 {
			t.Fatalf("%d confirmations cover %.6f but %.6f is at risk",
				got, float64(got)*e.BlockValue()/p.Safety, value)
		}
	})
}
