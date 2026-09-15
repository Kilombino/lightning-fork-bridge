package lnd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/paulscode/lightning-fork-bridge/lndrpc/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/lndrpc/routerrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// ErrNodeContradiction is returned when a node reports something that cannot
// be true, such as a succeeded payment with no preimage. It is deliberately
// not translated into a payment state: the bridge should stop rather than pick
// one of two readings when the node's own answer is incoherent.
var ErrNodeContradiction = errors.New("node reported an impossible payment")

// Outgoing drives the node the bridge pays on.
type Outgoing struct {
	router routerrpc.RouterClient
	main   lnrpc.LightningClient
}

// NewOutgoing wraps a connection.
func NewOutgoing(conn grpc.ClientConnInterface) *Outgoing {
	return &Outgoing{
		router: routerrpc.NewRouterClient(conn),
		main:   lnrpc.NewLightningClient(conn),
	}
}

var _ node.Outgoing = (*Outgoing)(nil)

// Pay sends a payment and waits for it to resolve.
//
// It returns an in-flight payment, not an error, when the attempt runs out of
// time. That distinction is the point: an HTLC that has left is out there
// whatever this call reports, and the only safe reading of "I stopped waiting"
// is "I do not know yet".
func (o *Outgoing) Pay(ctx context.Context, req node.PayRequest) (node.Payment,
	error) {

	if req.Invoice == "" {
		return node.Payment{}, fmt.Errorf("%w: no invoice to pay", ErrConfig)
	}
	if req.CLTVLimit == 0 {
		return node.Payment{}, fmt.Errorf("%w: no CLTV limit; the incoming "+
			"leg is sized against this and an unbounded route could outlive "+
			"it", ErrConfig)
	}
	if req.CLTVLimit > 1<<31-1 {
		return node.Payment{}, fmt.Errorf("%w: CLTV limit %d does not fit",
			ErrConfig, req.CLTVLimit)
	}
	if req.Timeout <= 0 {
		return node.Payment{}, fmt.Errorf("%w: no timeout; an attempt that "+
			"never gives up holds the incoming HTLC until it expires",
			ErrConfig)
	}

	// The stream gets its own context, cancelled on the way out, so that
	// returning early does not leave it open for the life of the caller's.
	//
	// Cancelling it does not cancel the payment. An HTLC already in flight
	// resolves on its own schedule whatever this process does, which is why
	// the only way to learn its fate is to look it up, and why nothing here
	// may conclude failure from having stopped listening.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := o.router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: req.Invoice,
		TimeoutSeconds: int32(req.Timeout.Seconds()),
		FeeLimitMsat:   int64(req.MaxFeeMsat),
		CltvLimit:      int32(req.CLTVLimit),
	})
	if err != nil {
		return node.Payment{}, fmt.Errorf("sending: %w", err)
	}

	// Anything short of a terminal answer is in flight. Starting from that
	// rather than from the zero value means a stream that dies early cannot
	// be read as "never started".
	last := node.Payment{State: node.PaymentInFlight}
	for {
		update, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return last, nil
			}

			// The stream broke or the deadline passed. The payment is not
			// known to have failed, and saying so would let the caller
			// cancel the incoming claim against an HTLC still in flight.
			return last, nil
		}

		pmt, err := translatePayment(update)
		if err != nil {
			return node.Payment{}, err
		}
		last = pmt

		switch pmt.State {
		case node.PaymentSucceeded, node.PaymentFailed:
			return pmt, nil
		}
	}
}

// LookupPayment reports what a payment is doing, without waiting for it.
func (o *Outgoing) LookupPayment(ctx context.Context, hash node.Hash) (
	node.Payment, error) {

	// The stream is closed as soon as one update has been read. Without
	// in-flight updates the node would hold the stream open until the payment
	// resolved, which is a wait, not a lookup, and recovery calls this for
	// every unfinished swap in turn.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The NotFound mapping below is an assumption about a particular node at
	// a particular version, not a guarantee: it is what lnd returns for a
	// payment it has never initiated. If a node signalled that some other
	// way, this returns an error instead, the driver reports that the swap
	// needs an operator, and nothing is lost but an operator's time — which
	// is the right direction for an assumption to fail in. Lab experiment
	// S14 confirms it.
	stream, err := o.router.TrackPaymentV2(ctx, &routerrpc.TrackPaymentRequest{
		PaymentHash:       hash[:],
		NoInflightUpdates: false,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return node.Payment{State: node.PaymentNone}, nil
		}

		return node.Payment{}, fmt.Errorf("tracking %x: %w", hash, err)
	}

	update, err := stream.Recv()
	if err != nil {
		// NotFound here means the node has never seen this hash, which is a
		// real answer after a restart and not an error: it is how the driver
		// tells "never sent" apart from "sent and lost track of", and the
		// difference is one it refuses to guess at.
		if status.Code(err) == codes.NotFound {
			return node.Payment{State: node.PaymentNone}, nil
		}

		return node.Payment{}, fmt.Errorf("tracking %x: %w", hash, err)
	}

	return translatePayment(update)
}

// translatePayment is the other translation worth reading twice.
func translatePayment(p *lnrpc.Payment) (node.Payment, error) {
	out := node.Payment{FeeMsat: uint64(p.GetFeeMsat())}

	switch p.GetStatus() {
	case lnrpc.Payment_SUCCEEDED:
		pre, err := preimageFromHex(p.GetPaymentPreimage())
		if err != nil {
			// A succeeded payment with no usable preimage is a node
			// contradiction, and the preimage is the entire proceeds of the
			// swap: it is what claims the incoming HTLC. Reporting success
			// without it would move the swap to a state whose only action
			// cannot be carried out.
			return node.Payment{}, fmt.Errorf("%w: succeeded but %w",
				ErrNodeContradiction, err)
		}
		out.State = node.PaymentSucceeded
		out.Preimage = pre

	case lnrpc.Payment_FAILED:
		// The only status that lets the bridge give up its incoming claim.
		// It means every HTLC has resolved, which is why nothing else may
		// map here.
		out.State = node.PaymentFailed

	case lnrpc.Payment_IN_FLIGHT, lnrpc.Payment_INITIATED:
		out.State = node.PaymentInFlight

	default:
		// Including Payment_UNKNOWN. A status this build does not recognise
		// is treated as in flight, because the two wrong answers are not
		// equally wrong: waiting on a payment that is over costs time, while
		// concluding failure on one that is not costs the swap.
		out.State = node.PaymentInFlight
	}

	return out, nil
}

func preimageFromHex(s string) (node.Preimage, error) {
	var pre node.Preimage
	if s == "" {
		return pre, errors.New("returned no preimage")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return pre, fmt.Errorf("returned an unreadable preimage %q", s)
	}
	if len(b) != len(pre) {
		return pre, fmt.Errorf("returned a %d byte preimage", len(b))
	}
	copy(pre[:], b)

	var zero node.Preimage
	if pre == zero {
		return pre, errors.New("returned an all-zero preimage")
	}

	return pre, nil
}

// BlockHeight is the tip this node sees.
func (o *Outgoing) BlockHeight(ctx context.Context) (int32, error) {
	return blockHeight(ctx, o.main)
}

// Check confirms the node answers and is synced to its chain. See
// Incoming.Check for why it is worth calling at startup.
func (o *Outgoing) Check(ctx context.Context) error {
	if _, err := o.BlockHeight(ctx); err != nil {
		return fmt.Errorf("outgoing node: %w", err)
	}

	return nil
}

// ErrInvoice is returned for a payment request the bridge will not act on.
var ErrInvoice = errors.New("payment request is not usable")

// Decode reads a payment request using the node that will pay it.
func (o *Outgoing) Decode(ctx context.Context, invoice string) (node.Decoded,
	error) {

	if invoice == "" {
		return node.Decoded{}, fmt.Errorf("%w: empty", ErrInvoice)
	}

	req, err := o.main.DecodePayReq(ctx, &lnrpc.PayReqString{PayReq: invoice})
	if err != nil {
		return node.Decoded{}, fmt.Errorf("%w: %w", ErrInvoice, err)
	}

	var out node.Decoded
	h, err := hex.DecodeString(req.GetPaymentHash())
	if err != nil || len(h) != len(out.Hash) {
		return node.Decoded{}, fmt.Errorf("%w: payment hash %q",
			ErrInvoice, req.GetPaymentHash())
	}
	copy(out.Hash[:], h)

	msat := req.GetNumMsat()
	if msat < 0 {
		return node.Decoded{}, fmt.Errorf("%w: negative amount %d", ErrInvoice,
			msat)
	}
	out.AmountMsat = uint64(msat)

	cltv := req.GetCltvExpiry()
	if cltv < 0 || cltv > 1<<31-1 {
		return node.Decoded{}, fmt.Errorf("%w: CLTV delta %d", ErrInvoice,
			cltv)
	}
	out.CLTVDelta = uint32(cltv)

	// Timestamp plus expiry, because an invoice's expiry is relative to when
	// it was created and the bridge cares about the wall-clock moment.
	if ts := req.GetTimestamp(); ts > 0 {
		out.Expiry = time.Unix(ts, 0).Add(
			time.Duration(req.GetExpiry()) * time.Second)
	}

	out.Destination = req.GetDestination()
	out.Description = req.GetDescription()

	return out, nil
}
