# Human waits and stall detection

The dispatcher and runview alert manager both consult
`store.HasBlockingHumanWait` before treating a silent run as stalled. The
observer reads persisted state; the waiting worker does not emit fake progress.

A running `await_answers` node records a per-invocation token, its node ID, and
the expiry derived from its mandatory timeout. The token is written only after
the node finds pending async questions and enters its synchronous wait. An
`interaction: async` agent with outstanding questions has no such marker and
remains subject to normal stall detection.

Answer, timeout, and cancellation remove that invocation's token. Parallel
waits have independent tokens. Status transitions discard execution-local
markers; stale full-document saves cannot overwrite granular writes because
they participate in the existing run-version fence. Additions require a running
run, and deleted runs cannot be resurrected. FS and Mongo implement the same
contract. Storage failures surface as execution errors; a failed cleanup cannot
leave an exemption beyond the original timeout.

A cleanup write failure also fails the node after answers were collected:
continuing into the next node with stale proof could hide its genuine stall
for the remaining sync timeout, possibly hours. The answers remain durable in
the interaction store and are collected again on retry without a second human
prompt. This favors a visible, recoverable storage failure over a hidden stall.
Cleanup uses a separate five-second context; the filesystem writer retains its
existing ten-second file-lock ceiling rather than honoring context cancellation.

The shared lookup also follows `SubbotChildren` to a paused descendant or an
active descendant sync point. Terminal subtrees are ignored. No proof, an
expired marker, unreadable records, cycles, or exhaustion of the 256-record
probe budget never provide an exemption by themselves. A valid proof found
elsewhere in the visited tree still qualifies, as in the original dispatcher
oracle.

Human-wait lookups run outside the event observer's mutex with a five-second
context deadline cancelled by manager shutdown. The candidate is checked again after
the lookup, so progress arriving during a read prevents a stale alert. When
the wait ends, the next poll applies the existing stall threshold to the last
real progress event; the wait does not reset the watermark.

The service's internal alert callbacks bootstrap identity from the known run
ID, then scope descendant reads and the persisted health event to that run's
tenant. A paused child belonging to another tenant cannot exempt the root.

Regression coverage includes both original failures, answer/cancel/timeout,
write and cleanup failure, parallel token isolation, stale saves, lifecycle
clearing and deletion on both stores, descendant resume without a parent event,
and real runview wiring across the alert poll interval. Negative cases keep
genuinely silent runs eligible for alerts and cancellation.
