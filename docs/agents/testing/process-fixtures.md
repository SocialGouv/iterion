# Process fixtures — capture, checkout roots and installed binaries

Evidence for the [test-author rules](testing.md#testing-patterns), linked from the
[machine-dependent test register](flakes.md).

| Test / check | Machine class | Evidence (log / SHA) | Disposition | Re-check |
|---|---|---|---|---|
| Process stdout/stderr capture helpers | Loaded Linux host with reduced pipe capacity | [#2989](https://github.com/SocialGouv/iterion/issues/2989): projectenv snapshot hung 20 minutes writing refusals before the helper began reading; 15 pipe sites in 11 test files audited | **Corrected, verified 2026-11-03**: every capture uses `internal/stdiotest`, which drains concurrently and closes both ends, including on panic/Goexit. A >2 MiB stdout/stderr witness preserves every byte; delaying the reader until after the callback fails its own progress assertion. Repeated intervals and restoration are tested under `-race`. | 2026-11-10, and on capture helper changes |
| Repository sweeps rooted in compile-time paths | A test binary built in another checkout | [#2916](https://github.com/SocialGouv/iterion/issues/2916), T45: a sweep read the primary checkout's private run state; `f636b11b7` moved the last two sites to `sweepwalk.ModuleRoot` | **Verified 2026-11-03**: no `runtime.Caller` calls remain in repository tests. A compiled helper invoked from a different, owned module finds only that execution tree's marker; replacing its CWD origin with `runtime.Caller` fails the root assertion. Package and test-author docs name the rule. | 2026-11-10, and on sweep root changes |

## Worked example: an installed binary defeats an empty PATH

`TestLaunchDetached_NamesTheDroppedOverrides` expects a detached launch to
refuse after logging dropped overrides. An empty PATH cannot prove that: the
resolver also searches fixed install paths, the operator's home among them. The
fix supplies an empty install-candidate seam, an owned HOME and an empty
ITERION_BIN; the production resolver and its sandbox caller share that seam — a
second resolver would reopen the class.

The mutation in `e40def071` disables the seam and drops the HOME pin on a
machine with an installed binary: the test fails its own expected-refusal
assertion (a process-leak warning only supports it). Re-check on a machine with
the installation and on an isolated worker, recording SHA and elapsed time.
