// Package lnd connects the bridge's node interfaces to real LND-family nodes
// over gRPC.
//
// Both sides speak the same RPCs: the Lightning Fork node on the BLAKE2b chain
// and the stock LND on Bitcoin differ in how blocks are hashed, not in their
// RPC surface, so one client drives both.
//
// Nothing here makes a decision. The adapters translate, and translating is
// where a wrong answer is most dangerous, because every safety rule above them
// is reasoning about what these methods report. Two translations carry that
// weight and are called out where they happen: a multi-part payment's expiry
// height is the minimum across its accepted HTLCs and not the invoice's
// nominal delta, and a payment is only reported failed when the node says
// failed, never when a call timed out or a stream broke.
package lnd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ErrConfig is returned for a connection that cannot be set up.
var ErrConfig = errors.New("node connection is not usable")

// maxMessage is the largest reply accepted. LND's defaults are generous and a
// node with many channels can exceed the gRPC default of 4 MiB on calls the
// bridge does not make, but the cost of allowing more is nothing.
const maxMessage = 50 * 1024 * 1024

// Config is how to reach one node.
type Config struct {
	// Address is host:port of the node's gRPC listener.
	Address string

	// TLSCertPath is the node's self-signed certificate, used as the only
	// root the client will accept. LND generates it at first start.
	TLSCertPath string

	// MacaroonPath is the macaroon to present. The bridge needs invoice and
	// payment permissions; admin.macaroon works and is more than it needs.
	MacaroonPath string

	// Timeout bounds the initial connection. Zero means 30 seconds.
	Timeout time.Duration
}

// macaroonCredential presents a macaroon on every call.
//
// LND expects it as hex in the "macaroon" metadata key. This is the whole of
// what the bridge needs from lnd's macaroons package, so it is written out
// here rather than importing a node's worth of dependencies for one header.
type macaroonCredential struct {
	hex string
}

func (m macaroonCredential) GetRequestMetadata(context.Context, ...string) (
	map[string]string, error) {

	return map[string]string{"macaroon": m.hex}, nil
}

// RequireTransportSecurity is true because a macaroon is a bearer token: sent
// in the clear it is the whole of the node's authority, handed to whoever is
// listening.
func (m macaroonCredential) RequireTransportSecurity() bool { return true }

// Dial connects to a node.
//
// It returns as soon as the configuration is usable, which is not the same as
// the node being reachable: gRPC connects lazily, so a wrong address or a node
// that is down shows up on the first call rather than here. Use the adapters'
// Check to find that out at startup instead of on the first swap.
func Dial(ctx context.Context, cfg Config) (*grpc.ClientConn, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("%w: no address", ErrConfig)
	}

	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrConfig,
			cfg.TLSCertPath, err)
	}

	mac, err := os.ReadFile(cfg.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrConfig,
			cfg.MacaroonPath, err)
	}
	if len(mac) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrConfig, cfg.MacaroonPath)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroonCredential{
			hex: hex.EncodeToString(mac),
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMessage),
			grpc.MaxCallSendMsgSize(maxMessage),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: dialling %s: %w", ErrConfig, cfg.Address,
			err)
	}

	return conn, nil
}
