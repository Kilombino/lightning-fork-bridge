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

`margin` and `confirm` exist because block counts mean different things on the
two chains. The BLAKE2b chain has run between 1.3x and 19x faster than its ten
minute target, and a block on it costs about 1/325th as much to produce, so
neither HTLC timing nor reorg depth can be reasoned about in blocks.

## Status

The logic that decides things is built and tested. What is not built is the
gRPC to real nodes, persistence, and the quote and payout surfaces above them.

Nothing here has moved money. The order was deliberate: the parts that lose
funds when they are wrong are the parts that can be tested exhaustively
without a node, so they were written first and reviewed adversarially before
anything could call them.
