# lightning-fork-bridge

Swaps between the Bitcoin BLAKE2b chain's Lightning network and Bitcoin's.

A user pays a `lnbc…` invoice from a Lightning Fork node, or is paid by someone
on the SHA256 chain, without either side giving custody to anyone: both
networks use the same HTLC construction and the same SHA256 payment hash, so a
hold invoice on the receiving chain and a payment on the sending chain settle
or fail together.

This daemon drives two nodes over gRPC, a Lightning Fork node for the BLAKE2b
chain and a stock LND for Bitcoin. Nothing here runs on both chains.

## Packages

| Package | What it decides |
| --- | --- |
| `chainrate` | How fast a chain is producing blocks, and the bounds a swap may rely on |
| `margin` | Whether an outgoing payment is safe to commit, and how much incoming CLTV a swap needs |
| `confirm` | How many confirmations a value at risk needs, priced in forgone block subsidy |
| `swap` | The state machine for one swap, and the rule that keeps a bridge solvent |
| `node` | What the bridge needs from the two Lightning nodes, as interfaces |
| `driver` | When to pay, and what a swap interrupted mid-flight should do |
| `rate` | What rate to quote, or when to refuse to quote at all |
| `store` | The durable record, which is what recovery reads and therefore what recovery believes |
| `lnd` | The two real nodes, over gRPC |
| `lndrpc` | Generated protobuf clients, see its own README |

`margin` and `confirm` exist because block counts mean different things on the
two chains. The BLAKE2b chain has run between 1.3x and 19x faster than its ten
minute target, and a block on it costs about 1/325th as much to produce, so
neither HTLC timing nor reorg depth can be reasoned about in blocks.

`store` is about ordering more than storage. A swap's state has to be durable
*before* the action it authorises is taken, because after a crash the record is
the only thing that says whether an outgoing payment might be in flight. Write
it afterwards and a swap whose payment has already left comes back reading as
merely funded, the bridge re-decides, the margin refuses because the crash cost
it time, and it cancels its claim on the money coming in.
`TestTheOrderingRuleIsWhatPreventsTheLoss` runs exactly those facts twice with
only that ordering changed, and gets opposite answers.

`lnd` holds two translations worth reading twice, because every rule above
them is reasoning about what they report. A multi-part payment's expiry height
is the **minimum** across its accepted HTLCs, not the invoice's nominal delta,
because the claim on the whole invoice dies with the earliest part. And a
payment is reported failed only when the node says failed: a broken stream, a
deadline, a status this build has never seen all come back as in flight,
because `OutgoingFailed` is the single event that lets the bridge give up its
incoming claim.

## Status

The logic that decides things is built and tested, the record it decides
against is built and tested, and the adapters to real nodes are built and
tested against an in-process gRPC server. What is not built is the event loop
that runs them, and the quote and payout surfaces above it. Nothing has been
run against a live node yet.

Nothing here has moved money. The order was deliberate: the parts that lose
funds when they are wrong are the parts that can be tested exhaustively
without a node, so they were written first and reviewed adversarially before
anything could call them.
