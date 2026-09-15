package swap

import (
	"errors"
	"fmt"
)

// Binding is what makes a swap's two legs one transaction rather than two
// hopeful payments.
//
// There are two kinds and they do not offer the same assurance. Conflating
// them is the most expensive mistake available here, so they are named rather
// than inferred, and every swap has to say which it is.
type Binding uint8

const (
	// HashLinked: both legs carry the same payment hash.
	//
	// The preimage that settles the outgoing leg is the only thing that can
	// claim the incoming one, so the bridge *cannot* take the incoming funds
	// without having delivered. That is not a rule the bridge follows, it is
	// a fact about what it holds, and it survives any bug in this program.
	//
	// This is the ordinary direction: a user hands over an invoice, the
	// bridge builds its hold invoice on that invoice's hash.
	HashLinked Binding = iota

	// Sequenced: the legs carry different payment hashes.
	//
	// This is what a payout looks like. The recipient's node issues the
	// outgoing invoice and chooses its own preimage, so there is no shared
	// hash to bind against, and the bridge knows the incoming preimage from
	// the moment it issues the hold invoice.
	//
	// **The protection is the order instead, and the order is enforced by
	// this program rather than by cryptography.** The incoming HTLC is held
	// until the outgoing leg has definitively succeeded, so the recipient is
	// paid before the bridge collects. A bridge that settled early would be
	// stealing, and nothing but this code stops it — which is precisely why
	// the state machine never emits SettleIncoming except out of Paid, and
	// why the driver refuses to settle from any other state.
	//
	// What the recipient is protected from is failure, not malice: if the
	// outgoing leg does not succeed the incoming HTLC is cancelled, the
	// payer's money goes back, and nobody is short. Malice is bounded by one
	// payout cycle and answered by people not using the bridge again.
	Sequenced
)

var bindingNames = map[Binding]string{
	HashLinked: "hash-linked", Sequenced: "sequenced",
}

func (b Binding) String() string {
	if n, ok := bindingNames[b]; ok {
		return n
	}

	return fmt.Sprintf("binding(%d)", uint8(b))
}

// ErrUnknownBinding is returned by ParseBinding for a name it does not know.
var ErrUnknownBinding = errors.New("unknown swap binding")

// ParseBinding turns a binding's name back into a Binding. Stored by name for
// the same reason states are: the numbers are positions in an iota block, and
// a record misread as the weaker binding would be trusted more than it earned.
func ParseBinding(name string) (Binding, error) {
	for b, n := range bindingNames {
		if n == name {
			return b, nil
		}
	}

	return 0, fmt.Errorf("%w: %q", ErrUnknownBinding, name)
}

// NeedsPreimageUpfront reports whether the bridge must already hold the
// incoming preimage when it accepts money.
//
// Sequenced swaps do: there is no later moment at which the preimage arrives,
// because the outgoing leg's preimage settles nothing on the incoming side. A
// sequenced swap without one could never be claimed, so the money would be
// accepted against a claim that was dead on arrival.
func (b Binding) NeedsPreimageUpfront() bool {
	return b == Sequenced
}
