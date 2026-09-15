package lnd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/paulscode/lightning-fork-bridge/lndrpc/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/lndrpc/routerrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// Live tests run against a real node and are skipped unless one is configured.
//
//	BRIDGE_LIVE_ADDR=172.19.0.6:10009 \
//	BRIDGE_LIVE_CERT=/tmp/s14/sha-tls.cert \
//	BRIDGE_LIVE_MACAROON=/tmp/s14/sha-admin.macaroon \
//	BRIDGE_LIVE_SERVERNAME=lnd-sha \
//	go test -run TestLive -v ./lnd/
//
// They are in the ordinary test file set rather than behind a build tag so
// that they compile on every run, because an experiment that stops compiling
// silently is one nobody notices has stopped being run.
func liveConn(t *testing.T) *grpc.ClientConn {
	t.Helper()

	addr := os.Getenv("BRIDGE_LIVE_ADDR")
	if addr == "" {
		t.Skip("set BRIDGE_LIVE_ADDR and friends to run against a real node")
	}

	conn, err := Dial(context.Background(), Config{
		Address:      addr,
		TLSCertPath:  os.Getenv("BRIDGE_LIVE_CERT"),
		MacaroonPath: os.Getenv("BRIDGE_LIVE_MACAROON"),
		ServerName:   os.Getenv("BRIDGE_LIVE_SERVERNAME"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

// TestLiveS14 is the experiment the design records as S14.
//
// Recovery's hardest case is a swap recorded as paying whose outgoing node has
// no record of the payment, because "never left" and "left and the node lost
// track of it" call for opposite actions and only one of them is recoverable.
// The adapter maps that to node.PaymentNone, and the driver turns PaymentNone
// into "stop and ask an operator".
//
// Two questions. First, what a real node actually answers for a hash it has
// never initiated — the adapter assumes codes.NotFound. Second, whether a
// payment record exists even when no HTLC ever left, which is the property
// that would make "no record" mean "never left" and let the answer eventually
// be automated instead of escalated.
func TestLiveS14(t *testing.T) {
	conn := liveConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out := NewOutgoing(conn)
	if err := out.Check(ctx); err != nil {
		t.Fatal(err)
	}

	info, err := lnrpc.NewLightningClient(conn).GetInfo(ctx,
		&lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("node: %s %s, height %d", info.GetAlias(), info.GetVersion(),
		info.GetBlockHeight())

	var never node.Hash
	if _, err := rand.Read(never[:]); err != nil {
		t.Fatal(err)
	}

	t.Run("a hash the node has never seen", func(t *testing.T) {
		// The raw answer first, so the finding is about the node rather
		// than about our mapping of it.
		stream, err := routerrpc.NewRouterClient(conn).TrackPaymentV2(ctx,
			&routerrpc.TrackPaymentRequest{PaymentHash: never[:]})
		var raw error
		if err != nil {
			raw = err
		} else {
			_, raw = stream.Recv()
		}
		t.Logf("S14(a) raw: code=%v message=%q", status.Code(raw), raw)

		if status.Code(raw) != codes.NotFound {
			t.Errorf("S14(a) FAILED: the adapter assumes NotFound for a "+
				"payment never initiated, this node says %v", status.Code(raw))
		}

		// Then our mapping of it, which is what recovery actually sees.
		pmt, err := out.LookupPayment(ctx, never)
		if err != nil {
			t.Fatalf("S14(a) the adapter errored instead of answering: %v", err)
		}
		if pmt.State != node.PaymentNone {
			t.Errorf("S14(a) the adapter reports %v, want none", pmt.State)
		}
		t.Logf("S14(a) PASS: a never-initiated hash reads as %v", pmt.State)
	})

	t.Run("a payment that failed before any HTLC left", func(t *testing.T) {
		// Sent to a valid public key with no route to it, so route-finding
		// fails and nothing is ever dispatched. If a record exists anyway,
		// it was written before the attempt rather than after it, which is
		// what makes "no record" mean "never left".
		dest, err := hex.DecodeString(
			"0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
		if err != nil {
			t.Fatal(err)
		}

		var hash node.Hash
		if _, err := rand.Read(hash[:]); err != nil {
			t.Fatal(err)
		}

		router := routerrpc.NewRouterClient(conn)
		stream, err := router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
			Dest: dest, AmtMsat: 1_000_000, PaymentHash: hash[:],
			FinalCltvDelta: 40, TimeoutSeconds: 10, FeeLimitMsat: 10_000,
		})
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
		for {
			update, err := stream.Recv()
			if err != nil {
				t.Logf("send stream ended: %v", err)

				break
			}
			t.Logf("send update: status=%v reason=%v", update.GetStatus(),
				update.GetFailureReason())
			if update.GetStatus() == lnrpc.Payment_FAILED ||
				update.GetStatus() == lnrpc.Payment_SUCCEEDED {

				break
			}
		}

		pmt, err := out.LookupPayment(ctx, hash)
		if err != nil {
			t.Fatalf("looking it up: %v", err)
		}
		t.Logf("S14(b) after a route-finding failure the node reports %v",
			pmt.State)

		if pmt.State == node.PaymentNone {
			t.Errorf("S14(b) FAILED: a payment this node definitely attempted " +
				"reads as never initiated, so PaymentNone cannot be taken to " +
				"mean an HTLC never left")
		} else {
			t.Logf("S14(b) PASS: the record exists even though no HTLC was " +
				"dispatched, so it is written before the attempt")
		}
	})
}
