// Package store is the durable record of every swap.
//
// The record is not bookkeeping. It is the input to recovery, and recovery is
// where a bridge loses money. After a crash the only thing that says whether an
// outgoing payment might be in flight is what was written to disk before it
// left; a swap whose record still says "funded" will be recovered by cancelling
// the incoming claim, which is exactly the wrong move if the outgoing leg
// actually went out. So the ordering matters more than the format: the state
// has to be durable before the action it authorises is taken, and the driver
// enforces that.
//
// Two implementations are here. Journal is the real one: an append-only file,
// fsynced per record, checksummed, replayed at open. Memory is for tests and
// refuses to pretend otherwise.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

var (
	// ErrNotFound is returned when no record exists for a hash.
	ErrNotFound = errors.New("no record for this payment hash")

	// ErrCorrupt is returned when the journal holds something that cannot be
	// read back. It is never repaired automatically: a damaged record in the
	// middle of the file means the bridge does not know what it did, and
	// guessing is worse than stopping.
	ErrCorrupt = errors.New("journal is corrupt")

	// ErrRecord is returned for a record that cannot be stored.
	ErrRecord = errors.New("record is not usable")
)

// Record is one swap, in the form that survives a restart.
//
// It holds what the decisions need and what an operator would have to
// reconstruct by hand otherwise. Amounts are on both sides because the pair is
// the only evidence of what rate was actually honoured.
type Record struct {
	// Hash appears on both chains and is the primary key.
	Hash node.Hash

	// State is where the machine has got to.
	State swap.State

	// Preimage is known once the outgoing leg succeeds. Until then it is
	// zero, and a zero preimage is why SettleIncoming refuses.
	Preimage node.Preimage

	// OutgoingCLTVLimit is the CLTV budget imposed on the outgoing payment,
	// in blocks of the outgoing chain. The incoming leg has to outlive it.
	OutgoingCLTVLimit uint32

	// Invoice is the outgoing invoice being paid, exactly as the payer
	// supplied it. The bridge never rewrites it: its payment hash is what the
	// incoming hold invoice was built on.
	Invoice string

	// IncomingMsat is what the payer owes on the incoming chain and
	// OutgoingMsat what leaves on the outgoing one. They are in different
	// units and the gap between them, at the quoted rate, is the spread.
	IncomingMsat uint64
	OutgoingMsat uint64

	// Rate is the outgoing-per-incoming price the quote was struck at, and
	// Spread the fraction charged over it. Kept because a quote that was
	// honoured at a rate nobody can reproduce later is indistinguishable from
	// one that was mispriced.
	Rate   float64
	Spread float64

	// Created and Updated are wall-clock, for operators and for ageing out
	// quotes. Nothing safety-critical is decided from them; block heights do
	// that.
	Created time.Time
	Updated time.Time

	// Note is free text. The states that need a human carry the reason here.
	Note string
}

// Valid reports whether a record can be stored. It is deliberately narrow:
// this is the last check before something becomes the thing a restart trusts.
func (r Record) Valid() error {
	var zeroHash node.Hash
	if r.Hash == zeroHash {
		return fmt.Errorf("%w: zero payment hash", ErrRecord)
	}
	if _, err := swap.ParseState(r.State.String()); err != nil {
		return fmt.Errorf("%w: %w", ErrRecord, err)
	}
	if r.Created.IsZero() {
		return fmt.Errorf("%w: no creation time", ErrRecord)
	}

	// A preimage must hash to the record's hash or it settles nothing, and a
	// record carrying the wrong one would send the driver to settle with a
	// value the node will reject, in the one state where a rejection is a
	// realised loss.
	var zeroPre node.Preimage
	if r.Preimage != zeroPre && r.Preimage.Hash() != r.Hash {
		return fmt.Errorf("%w: preimage does not hash to %x", ErrRecord,
			r.Hash)
	}

	return nil
}

// Store is the durable record. Put must not return until the record would
// survive the loss of the process and of the machine.
type Store interface {
	// Put writes a record, replacing any earlier one for the same hash. It
	// returns only once the write is durable.
	Put(ctx context.Context, r Record) error

	// Get reads the current record for a hash, or ErrNotFound.
	Get(ctx context.Context, hash node.Hash) (Record, error)

	// Pending returns every non-terminal record, oldest first. This is what a
	// restart works through.
	Pending(ctx context.Context) ([]Record, error)

	// Close releases the store.
	Close() error
}
