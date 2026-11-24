# Codemode run state

`call.store(key, value)` writes a JSON value to the run's state bank and
returns `true` on success. `call.load(key)` returns a fresh JavaScript value;
an absent key returns `undefined`, not the JSON value `null`.

```js
call.store("scan-summary", {count: 12});
call.load("scan-summary").count;
```

A later script in the same run can read the write. Parallel nodes and fan-out
branches share one bank; use distinct logical keys when they own different
results. A child run or fork with a new run ID starts empty. Retry, resume,
and rewind of the same run retain acknowledged writes. Rewind moves execution,
not this bank, and a later script failure does not roll back a committed write.
Deleting the run removes its bank on an upgraded store. During a Mongo
rollout, upgrade all processes that can delete runs before enabling bank
writes: an older process can leave the bank in the tombstone until pruning.

Keys are 1–128 UTF‑8 bytes, and each value is at most 16 KiB of JSON. The
run-wide defaults are 32 entries and 128 KiB (keys plus stored JSON), with
platform ceilings of 64 entries and 512 KiB. Workflow and run overrides use
`store_entries` and `store_bytes`; nodes cannot claim separate banks or quotas.
A replacement counts the new value instead of the old one. Lowering a quota
does not delete values. Both operations use an invocation's tool-call budget
and are synchronous; they are not entries for `call.parallel()`.

Values use JavaScript's JSON serialization rules: object properties with
`undefined` or function values are omitted, and unsupported array elements
and non-finite numbers serialize as `null`. A root without a JSON
representation or a string with an unpaired surrogate is refused.

Scripts can catch state errors via `error.code` (`state_unavailable`,
`state_invalid`, `state_quota`, or `call_budget`). An unacknowledged delegated
write can report `state_write_unconfirmed`: it
may still commit, so verify the value before retrying. The launcher honors
an earlier runner deadline, but explicit cancellation is not a rollback or a
confirmation that a queued request was withdrawn. Local argument validation
throws a `TypeError`. No filesystem path, credential, tenant ID, or run ID is
accepted from the script. The launcher owns the bank and its limits; a
sandbox runner does not get a local substitute. This does not establish
CM‑6 sandbox-crossing certification.

The bank is not a secret store: a later script in the same run can load it,
and a script that returns a loaded value exposes it as script output. Do not
store credentials or data that another node in the run must not read.
