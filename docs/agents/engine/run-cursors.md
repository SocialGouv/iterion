# Event stream cursors

Companion to [run-streams.md](run-streams.md): sequence boundaries do not change retained-origin or subscription-lifetime contracts.

`Source.SubscribeEvents(ctx, id, fromSeq)` remains inclusive. The additive `EventCursorSource` capability accepts `EventCursor{Seq, After}`: `After=false` includes Seq, while `After=true` requests persisted events strictly after it. File, in-process service, and both Mongo constructors implement this capability. The WS lazy subscription uses the snapshot's last sequence as an exclusive cursor; explicit history replay stays inclusive, and an empty snapshot preserves the requested starting sequence.

After `MaxInt64`, a finite persisted replay is empty. The source can still wait for completion and deliver its existing out-of-band alerts; this is not an EOF shortcut for an active run. In-process live alerts retain the existing EventAlert type exemption and high-water behavior. Sequence comparisons do not establish event origin or authorization.

`SubscribeEventsCursor` also supports old Source implementations when the equivalent inclusive anchor is representable. An old implementation lacking the capability receives `ErrEventCursorUnsupported` for after-Max, rather than a wrapped, saturated or silently inclusive request. Implementing the additive capability removes that limitation without changing the wire protocol.

`ReplayEventsCursor` reports a next cursor plus `Found` and `LastSeq`, advancing only after successful delivery. The legacy `ReplayEvents` result still uses a predecessor when no rows were delivered. For an empty replay starting at `MinInt64`, that predecessor cannot be represented: it returns `ErrReplayProgressUnrepresentable`, joined with any original failure, and its integer result is nonauthoritative. New callers can use cursor progress for all int64 anchors.

This change addresses cursor arithmetic and progression. It does not repair the in-process source's pre-existing permanent-error handling, unbound `LoadRun` contexts or final-drain error policies, and it adds no provenance, epoch or cross-connection claim.
