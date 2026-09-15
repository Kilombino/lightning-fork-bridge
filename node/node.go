// Package node is what the bridge needs from the two Lightning nodes it
// drives, expressed as interfaces so the logic above can be tested without
// either of them.
//
// The incoming side is a Lightning Fork node on the BLAKE2b chain and the
// outgoing side a stock LND on Bitcoin, or the reverse for a swap going the
// other way. Nothing here is specific to either: a swap receives on one chain
// and pays on the other, and which is which is the caller's business.
package node

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrUnknownHash is returned when a node has no record of a payment hash.
var ErrUnknownHash = errors.New("node has no record of this payment hash")

// Hash is a payment hash. The same one appears on both chains, which is what
// makes a swap atomic.
type Hash [32]byte

// Preimage is what settles both legs.
type Preimage [32]byte

// InvoiceState is what a hold invoice on the incoming chain is doing.
type InvoiceState uint8

const (
	// InvoiceOpen: created and waiting to be paid.
	InvoiceOpen InvoiceState = iota

	// InvoiceAccepted: the HTLC is locked in and claimable with the
	// preimage. This is the state a hold invoice sits in while the bridge
	// works, and the state that makes the swap safe to pay out against.
	InvoiceAccepted

	// InvoiceSettled: claimed.
	InvoiceSettled

	// InvoiceCancelled: returned to the payer, or expired. Once here the
	// money cannot be claimed, whatever the bridge knows.
	InvoiceCancelled
)

var invoiceStateNames = map[InvoiceState]string{
	InvoiceOpen: "open", InvoiceAccepted: "accepted",
	InvoiceSettled: "settled", InvoiceCancelled: "cancelled",
}

func (s InvoiceState) String() string {
	if n, ok := invoiceStateNames[s]; ok {
		return n
	}

	return fmt.Sprintf("invoice state(%d)", uint8(s))
}

// Claimable reports whether the incoming HTLC can still be settled.
func (s InvoiceState) Claimable() bool {
	return s == InvoiceAccepted
}

// Invoice is what the incoming node says about a hold invoice.
type Invoice struct {
	State      InvoiceState
	AmountMsat uint64

	// ExpiryHeight is the chain height at which the incoming HTLC stops
	// being claimable. Together with the current height it gives the blocks
	// of CLTV remaining, which is what the margin check needs.
	//
	// A multi-part payment arrives as several HTLCs with different expiry
	// heights, and the whole claim dies with the earliest of them, so an
	// implementation MUST report the minimum across the accepted HTLCs and
	// not the invoice's nominal delta. Reporting anything later overstates
	// how long the bridge has to claim, which is the direction that loses
	// money.
	//
	// It is zero while no HTLC is locked in.
	ExpiryHeight int32
}

// PaymentState is what an outgoing payment is doing.
type PaymentState uint8

const (
	// PaymentNone: the node has no record of it.
	PaymentNone PaymentState = iota

	// PaymentInFlight: one or more HTLCs are outstanding and the outcome is
	// not known. Nothing may be concluded from this.
	PaymentInFlight

	// PaymentSucceeded: the preimage is known.
	PaymentSucceeded

	// PaymentFailed: definitively failed, every HTLC resolved. A payment
	// whose outcome is merely unknown must never be reported as this.
	PaymentFailed
)

var paymentStateNames = map[PaymentState]string{
	PaymentNone: "none", PaymentInFlight: "in flight",
	PaymentSucceeded: "succeeded", PaymentFailed: "failed",
}

func (s PaymentState) String() string {
	if n, ok := paymentStateNames[s]; ok {
		return n
	}

	return fmt.Sprintf("payment state(%d)", uint8(s))
}

// Payment is what the outgoing node says about a payment.
type Payment struct {
	State    PaymentState
	Preimage Preimage
	FeeMsat  uint64
}

// HoldInvoice is what the bridge asks the incoming node to create.
type HoldInvoice struct {
	Hash       Hash
	AmountMsat uint64

	// CLTVDelta is the minimum CLTV the payer must leave on the final hop,
	// in blocks of the incoming chain. It comes from margin, which sizes it
	// in wall-clock and converts, so it is usually far larger than a
	// same-chain invoice would ask for.
	CLTVDelta uint32

	// Expiry is how long the invoice may go unpaid.
	Expiry time.Duration

	// Memo is what the payer sees.
	Memo string
}

// PayRequest is what the bridge asks the outgoing node to send.
type PayRequest struct {
	// Invoice is the destination invoice, as the payer supplied it. The
	// bridge never rewrites it: its payment hash is what the incoming hold
	// invoice was built on, and settling requires this exact preimage.
	Invoice string

	// MaxFeeMsat caps routing fees.
	MaxFeeMsat uint64

	// CLTVLimit caps the total CLTV of the route, in blocks of the outgoing
	// chain. Keeping it small is the cheapest way to shrink what the
	// incoming leg has to outlive.
	CLTVLimit uint32

	// Timeout abandons the attempt. It bounds how long the payment can sit
	// in flight, but not how long an HTLC that has already left can take to
	// resolve on chain, which is what CLTVLimit bounds.
	Timeout time.Duration
}

// Incoming is the node the bridge receives on.
type Incoming interface {
	// AddHoldInvoice creates an invoice that will not settle until told to.
	AddHoldInvoice(ctx context.Context, req HoldInvoice) (string, error)

	// LookupInvoice reports what a hold invoice is doing. It returns
	// ErrUnknownHash if the node has never seen it.
	LookupInvoice(ctx context.Context, hash Hash) (Invoice, error)

	// SettleInvoice claims the HTLC. It is idempotent: settling an already
	// settled invoice succeeds.
	SettleInvoice(ctx context.Context, preimage Preimage) error

	// CancelInvoice returns the HTLC. It is idempotent.
	CancelInvoice(ctx context.Context, hash Hash) error

	// BlockHeight is the tip this node sees, used to turn an expiry height
	// into blocks remaining.
	BlockHeight(ctx context.Context) (int32, error)
}

// Outgoing is the node the bridge pays on.
type Outgoing interface {
	// Pay sends a payment and blocks until it resolves or the request's
	// timeout elapses. A timeout is not a failure: the payment may still be
	// in flight, and LookupPayment is the only way to find out.
	Pay(ctx context.Context, req PayRequest) (Payment, error)

	// LookupPayment reports what a payment is doing. Unlike the incoming
	// side it does not error on an unknown hash, because "never started" is
	// a meaningful answer after a restart.
	LookupPayment(ctx context.Context, hash Hash) (Payment, error)

	// BlockHeight is the tip this node sees.
	BlockHeight(ctx context.Context) (int32, error)
}
