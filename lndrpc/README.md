# lndrpc

Generated gRPC clients for the two nodes the bridge drives.

## Why generated rather than imported

`github.com/lightningnetwork/lnd/lnrpc/invoicesrpc` and `.../routerrpc` are not
client libraries. They are the server implementations, and importing them pulls
in `channeldb`, `routing`, `htlcswitch`, `sweep` and most of the rest of lnd —
a node's worth of dependencies, and a hard coupling to one node's internals, in
a daemon that only needs to make about eight calls.

Generating from the `.proto` files instead gives a client whose entire
dependency tree is gRPC and protobuf. The protos are the stable contract
between the bridge and the nodes; lnd's Go packages are not.

## One client, both chains

The Lightning Fork blake2b branch keeps `lightning.proto`, `invoices.proto` and
`router.proto` **byte-identical to upstream v0.21.3-beta**. The fork changed how
blocks are hashed, not the RPC surface. So the same generated client drives the
BLAKE2b node and the stock LND on Bitcoin, and there is no second set of
bindings to keep in step.

`make check-protos` re-verifies that, because it is an assumption rather than a
guarantee: if the fork ever changes one of these files, a bridge built against
stale bindings would misread whatever moved.

## Regenerating

    make protos

Sources are vendored in `proto/`, copied from the fork with only the
`go_package` option rewritten. `PROTO_SOURCE` in the Makefile records where
they came from.
