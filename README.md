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
| `quote` | Turning "pay this invoice" into a swap, or into a refusal |
| `inventory` | What to charge, from what the bridge is holding |
| `runner` | Driving a swap to completion, and what to retry versus what to stop for |
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

`runner` holds two rules. One goroutine per payment hash, ever: a swap's
safety is a sequence — read the nodes, decide, record, act — and two goroutines
interleaving it can gate against one reading and pay against another. And a
split between failures that must be retried and failures that must not.
A node that did not answer says nothing about the swap, and abandoning a live
HTLC over a blip is how one expires; an inconsistency, or anything the bridge
has said it must not decide alone, must not be repeated at all.

`quote` never chooses the payment hash. It comes from the invoice the user
asked to have paid, read by **the node that will pay it** rather than by a
second parser in this process, because a parser that disagreed by one field
would have the bridge hold an invoice for one hash while the node paid another.
The rest of the package is refusals: an invoice that expires before the
incoming leg could be funded, a routing budget wider than the spread it comes
out of, an incoming amount rounded in the payer's favour on every swap, a
second hold invoice for a hash that already has one.

`inventory` exists because the number the design most wanted to know in
advance — how much flow comes back the other way — is not knowable in advance.
It is demand to be created, not a fact to be measured, and a bridge that had to
predict it before it could quote would never start. So it is not predicted. The
spread is a function of the measured position instead: one-way flow drains the
outgoing side and prices itself at what one-way flow costs (3% at the default
policy), balanced flow never drains it and sits at the floor (1%), and nobody
decided which it would be. A swap in the replenishing direction gets a discount
instead, because it is doing the bridge a favour it would otherwise pay a venue
for — capped at what that venue would have charged, and lifted rather than
zeroed when there is no venue at all, since flow is then the only mechanism
there is.

## Status

Built and tested: the logic that decides, the record it decides against, the
adapters to real nodes, the loop that drives a swap to completion, the quote
surface that creates one, and the pricing that decides what it costs.

**A swap has run end to end across two chains.** `bridgetest` drives the whole
stack against the lab's four nodes — a user on the BLAKE2b chain paying a
Bitcoin invoice, with real hold invoices, real HTLCs and a real journal on
disk. Both halves are covered: the settlement, and the refund when the outgoing
leg cannot be delivered.

Not built: the payout adapter, and the daemon that wires this behind a
configuration file. Nothing has run against mainnet.

Nothing here has moved money. The order was deliberate: the parts that lose
funds when they are wrong are the parts that can be tested exhaustively
without a node, so they were written first and reviewed adversarially before
anything could call them.
