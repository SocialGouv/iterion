# Run invocation preparation and termination

For durable format authority and storage boundaries, see
[run value formats](run-value-formats.md).
The staged store API is in [run birth](run-birth.md).

Before executor construction, `store.PrepareRunInvocation` selects one mode:
`ExistingRunInvocation` loads an existing run; `NewLegacyRunInvocation` creates
exclusively; `ReservedRunInvocation` consumes an explicit birth reservation.
Missing pickups never become births, and duplicate births are never adopted.
The reserved mode retains a validated receipt even with a publication error,
for retirement only. Invalid receipts grant no cleanup ownership. `OwnedBirth`
is an accounting fact, not execution permission. Normal exact births remain
disconnected pending their activation stages.

The CLI's explicit `--run-id` may select an existing run, as before. Its first
read captures both trust and format before compilation; the later pickup must
retain that binding. A disappeared pickup never becomes a new birth. An ID
observed absent is created exclusively after compilation, so a concurrent birth
is not adopted. Absence must be unambiguous: a not-found cause joined with a
format refusal, cancellation or infrastructure failure still fails the launch.

The returned context carries the original run ID and value format.
`BindRunValueFormat` captures it once; `LoadRunWithValueFormat` compares subsequent
loads with that original. An absent binding remains compatible with historical
store APIs but cannot stand in for a prepared invocation at `BuildExecutor`.
`RequireRunValueFormat` checks presence and intention; the builder also reloads
the durable run before credentials, hooks and mount preparation. Neither an
expected format nor a successful read grants execution ownership.

CLI, service, dispatcher and worker entries retain this context through
`Engine.RunPrepared`/`Resume`, including explicit reloads and automatic resumes. A child
has a separate prepared ID after the parent operations. Its launcher requires
the parent's existing binding before choosing the child's ID and format
intention; absent parent binding cannot silently downgrade an exact parent. A parent with exact format is typed-refused by
the currently legacy-only child birth path. A dryrun pass instead owns a
separate disposable legacy store; cancellation before birth is still reported
as a timed-out simulation, not a fabricated persisted run.

A format refusal stops failure recording, automatic retry and completion
projections about the successor. Service completion callbacks still return the
error to their waiting launcher; arbitrary callbacks supplied by embedders
remain their responsibility. Cancellation after a successfully acquired action
uses the existing bounded cleanup contexts while retaining the original
binding. A failed read preserves its original error even if the context was
cancelled while reading; a successful read still checks cancellation before
returning its value.

For an exclusive birth, `PreparedRunInvocation` also retains a private copy of
the returned ID, format, epoch, version, initial status and queued timestamp.
This observation is not an execution grant. `AbortPreparedRun` only attempts
to retire such a birth when the preparation error actually comes from the
current cancellation. Classified business errors, joined causes, ownership
loss and typed format refusals retain their priority. An existing pickup, a
duplicate create or legacy creator error has no birth to retire. Reserved
publication errors retain their receipt. Pure publication cancellation preserves
its classification; typed failures and ambiguous effects still take priority.

`RunOutcomeCASStore` predicates outcomes on version, epoch and original
running/queued status. Metadata-only retries may refresh the version while retaining the
original epoch, status and format binding. Another acquisition or terminal
transition stops the retirement attempt. Filesystem checks share the stable
metadata lock; Mongo applies the predicates to the same tenant-scoped write.
Missing capability is an error, never an unconditional status fallback.
`PreparedStopOptions` controls this cleanup: zero values choose three attempts
and five seconds; negative values remove the corresponding bound. Exhausted
metadata contention returns `ErrRunConflict`, distinct from a superseded entry.
A refusal or error does not claim that cancellation was persisted.

The launcher hands the preparation to `Engine.RunPrepared` and ends its own
retirement responsibility at that call. The engine handles cancellation through
its admission phase; there is no deferred birth cleanup spanning node execution.
Every child host takes `AcquireSubbotLease` before recording the parent link
and publishing a running child. The service also registers its Manager handle
before birth; registration and lock failures refuse launch. Returned setup errors
call `RetirePreparedRun` under that liveness signal, using the original birth
predicates and outcome CAS. Independent setup errors remain failures even if
cancellation arrives concurrently. Existing pickups and successors are not retired.
`AbortPreparedRun` retains its narrower cancellation contract. A process crash
releases the filesystem lock, or lets the cloud lease expire; the running child
then remains eligible for orphan reconciliation instead of parking forever queued.

Renewable locks expose `Refresh(context.Context)` and their effective
`LeaseTTL()`. Wrappers must preserve these capabilities. Refresh starts during
preparation; a separate deadline cancels the active context if an acknowledgement
is missing or late. Validity is measured conservatively from request start.
The refresher must honor context cancellation and is joined before unlock.
NATS schema setup never rewrites an existing lease bucket's configuration, and
acquisition refuses a TTL mismatch. Changing that shared TTL requires draining
holders and coordinating the bucket configuration. This is advisory liveness,
not fencing for already-started external effects or a suspended process.

The active pass releases its lease before deregistering its Manager handle and
before waiting on a human gate. The park uses the original parent's lifetime
plus the child's immutable birth binding, preserving parent cancellation without
borrowing the released lease's cancelled context.

A new prepared queued run without correction accounting uses the existing
`RunExecutionStore` acquisition and retains only that atomic response. Its
original queued identity is checked before choosing a status branch, and its
acquired epoch is retained through admission retries. Run masks inherited grants
before its first cancellable check; Resume also owns a separate entry carrier. This lifecycle acquisition creates no model
ownership, accounting reservation or credential authority. Prepared running,
existing pickups and the historical Run path retain their existing acquisition
paths; this integration does not certify their concurrent ownership. Cancellation
already present at entry is now a refusal before effects: it creates no row and
leaves an existing row untouched. This deliberately replaces the historical
filesystem path that could mark an existing run from an already cancelled
request without acquiring it. The birth retirement API applies only to a proved
exclusive birth, never to an existing pickup.

After execution has begun, an unclassified context failure from an active node
phase uses the existing context-stop handler. Its terminal writes retain the
original tenant, format and acquired authority while detaching cancellation;
no fresh epoch is adopted. A format or ownership refusal blocks later writes
and is returned to the launcher. A failed required status write is returned as
an error, not as a claim that cancellation was persisted. Independently established
business or ambiguous-effect causes must remain discoverable beside any refusal.
A private observer retains refusals from nested best-effort event callbacks in
this synchronous sequence. The two checkpoint-failure entry points use the same
sequence while preserving their existing verdict, checkpoint and ordinary I/O
fallback policy. Nested publication borrows the original sequence and remaining
budget; it neither renews the deadline nor closes the owner's context. Workspace
boundary capture still receives the run's original execution context.
A handled result is tied to its invocation, so the two phase readers do not
restart the handler or its write budget. Separate invocations still handle their
own stop; no durable authority comes from this in-memory marker. Outside this
scope, existing best-effort event policies remain.
`WithTerminationWriteTimeout` controls the context-stop handler and these two
checkpoint-failure entry points: zero selects five seconds (the existing
context-stop default), and a negative duration removes the bound. It does not change preparation retirement,
branch collection grace or spend accounting. The budget governs context-aware
bookkeeping; it cannot interrupt a callback that ignores its context. The
sequence is not atomic across its writes: a legitimate earlier write can precede
a later refusal. This option and sequence do not yet cover every best-effort
observation or parallel cleanup path.

A replacement subbot inherits the previous child's correction allowance and
terminal decision, including a typed control cause. It does not inherit that
child's receipts, billed usage, generation or execution authority: those stay
in their original run. A strict correction-budget increase may reopen a
candidate rejection under the normal policy, without resetting its consumed
attempts. Existing children retain their own ledger.

This checkpoint does not make every resource lifetime atomic. Raw run-log/PID
and sandbox attachment paths, delayed handles, complete cloud publication and
transport activation are separate pending stages. A validated load does not
hold a lock across all later external effects, and the prepared-entry checks add no general same-format ABA guarantee or
fence for effects after admission. Readers must be deployed before old
writers are drained and new exact births are enabled.
