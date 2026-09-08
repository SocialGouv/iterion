# Test helpers end with their test

Issue #956 exposed a sentinel loop in the dispatcher subbot lock fixture:
writing the release file in `t.Cleanup` did not join the dispatch. The workspace
could disappear before the shell saw the file, leaving it polling forever.
The fixture now cancels the dispatch context and joins it before its runner and
temporary directories are cleaned up. The tool executor's process-group
cancellation from #955 then reaches both the shell and its children.

The CLI, runtime and cloud-runner fake shared sandboxes also use
`proc.TerminateGroupOnCancel` when constructing host commands. A fake sandbox's
`Exec` must have the same lifetime contract as the real host driver.

## Suite guard

`internal/proctest.NoProcessLeaks` wraps TestMain in dispatcher, runview, runner,
runtime, CLI and E2E. On Linux it makes the test binary a child subreaper before
running the suite: an orphaned descendant is adopted by this binary instead of
escaping to init ([Linux subreaper semantics](https://man7.org/linux/man-pages/man2/PR_SET_CHILD_SUBREAPER.2const.html)).
After all test cleanups, it checks every OS thread's direct children, reports
survivors, kills only its own children and repeats as grandchildren are
adopted. Already-exited adopted children are reaped. A leak makes a successful
suite fail, while an existing failure exit code is preserved.

The guard probes its child scanner before enabling adoption. If the kernel
does not expose `/proc/self/task/*/children` or refuses the subreaper option,
it prints `proctest: leak guard unavailable (...)` and still runs the suite,
preserving its result. Errors after successful activation remain hard
failures. The guard's own orphan fixtures skip after a clean subprocess
detects this unsupported environment, before intentionally spawning an orphan;
the other package suites continue to run.

"Cleanups returned" is not "the kernel finished the teardown", so a child is
only a leak once it is **still alive after a settle window** — 500 ms, or
`ITERION_PROCTEST_SETTLE`. A helper a cleanup signalled without joining, or one
a test joins by proxy (a pidfile disappearing), gets to run its exit path and is
forgiven — counted on stderr as `proctest: forgave N settling child(ren)`, never
killed inside the window. The window is a ceiling, not a wait: a suite with no
children breaks out immediately, and a child that exits at 30 ms is forgiven at
30 ms. Survivors past it are still reported and reclaimed, so the canary the
guard exists for is unchanged. The window and the tracking are per process,
keyed on PID *and* start time, so a recycled PID cannot inherit an expired one.

The guard never reaps while tests execute, where os/exec owns Wait. It never
scans or signals another session's process tree: historical orphans and sibling
package tests are outside its ancestry. Its five-second bound, which starts
after the settle window, covers reclaiming a reported leak; when that bound
expires the guard names and kills whatever is still alive before failing, so a
descendant adopted late — inside the last settle window of the budget, before
it could earn a verdict of its own — is still reported and signalled. This
deadline path fails the suite and is a final best-effort kill, not proof that
all descendants were joined: only the normal ECHILD exit establishes that
none remain. Fixture cancellation is portable; the
orphan-adoption guard is Linux-only. Like other TestMain postconditions it
requires m.Run to return; a forced kill of the test binary cannot execute the
postcondition.

## Shell and helper sweep

The sweep used `exec.Command`, its `CommandContext` and `osexec` aliases, plus
sentinel loops and long-lived DSL tool recipes in `_test.go` files. Each direct
shell family and the relevant process-owning fixtures have these dispositions:

| Sites | Disposition |
|---|---|
| `dispatcher/engine_runner_subbot_test.go` sentinel | Cancel and join before cleanup; dispatcher suite guarded. |
| `runview/subbot_reconcile_test.go` sentinel and `subbot_child_control_test.go` sleeps | Service Stop/Cancel owns the run and joins it; tool group cancellation reaches helpers; runview suite guarded. |
| `runner/subbot_test.go`, `runtime/sandbox_shared_test.go`, `cli/subbot_shared_sandbox_test.go` fake sandboxes | Add group cancellation; all three suites guarded. |
| `runner/loop_checkpoint_test.go` three shell invocations | Synchronous finite checkpoint scripts; Output joins each before assertions/TempDir cleanup. |
| `backend/model/executor_tool_escaping_test.go` four shell invocations | Synchronous finite quoting recipes; Run/Output joins. |
| `backend/model/executor_tool_test.go` fake copy sandbox | Command runs through the real tool-executor cancellation wrapper; fake Exec only records calls and starts no process. |
| `backend/delegate/cliagent_test.go` and `askuser_http_test.go` sandbox commands | Real CLI backend applies group cancellation before starting these fake commands. |
| `backend/delegate/pisdk/client_test.go` shell producer | Finite 5,000-line producer; Client.Close cleanup and exited-channel join. |
| `backend/delegate/claudesdk/process_test.go` process fixtures | Own process-group lifecycle under test; explicit reap/cancel paths; shell `exit 0` is finite. |
| `internal/proc/proc_unix_test.go` four shell invocations | Tests group-cancel versus detach semantics directly, with explicit cancellation/join; do not replace the primitive under test. |
| `internal/shellquote/shellquote_test.go` | Synchronous `printf` quoting check. |
| `sandbox/kubernetes/workspace_test.go` four shell invocations | Synchronous finite workspace/git fixup scripts. |
| `e2e/sec_audit_cap_findings_test.go`, `sec_audit_scan_health_test.go`, `sec_audit_deps_heuristics_test.go` | Synchronous local fixture post-processing, joined before assertions. |
| `e2e/feed_watch_test.go` shell plan and Python tools | Synchronous script execution, joined before parsing output/cleanup; no detached sentinel helper. |
| `e2e/mcp_server_test.go` | Explicit stdin-close and cmd.Wait stop; CLI runner lifecycles remain the subject of those E2E tests. |
| `e2e/cli_server_boot_test.go` server/runner processes | Join the server Wait goroutine during cleanup too, including early failure — bounded, since that join is only safe once cancellation kills the process GROUP (`cmd.Cancel`, mirroring `proc.TerminateGroupOnCancel`, which `e2e/` cannot import) and `cmd.WaitDelay` bounds a descendant that escapes it via `setsid`. Both are load-bearing: with a leader-only kill and no delay, a grandchild holding the inherited stdout/stderr pipes blocks `cmd.Wait` forever and the join hangs the whole E2E binary (measured on a standalone probe). Other runner commands are synchronously joined; E2E suite guarded. |
| `e2e/claw_tool_coverage_live_test.go` Xvfb | Live-only fixture explicitly kills and waits for the started process. |
| Other `true`, git/build and inspection commands | Finite synchronous helpers or no-op command factories; no sentinel/background lifetime. Git ownership is covered by gittest and #828/#870. |

## Failure-path evidence

A temporary probe made the lock fixture's shell report its PID, waited for it
to start, deleted its workspace and called Fatal before normal release. With
the old cleanup, the suite guard reported and reclaimed a surviving bash and
sleep. With cancel-and-join cleanup, only the deliberately injected test failure
remained: no process leak. The probe was removed afterward.

The guard's own subprocess tests check clean success, preservation of an
existing failure, detection/reaping of an orphan after its launcher exits, and
preservation of an existing failure when a leak is also found. A fifth case
covers the other side of the settle boundary — an orphan that exits well
inside a widened window is forgiven, silently, and still reaped — and asserts
the forgiveness COUNT, without which it would pass identically to the clean
case even with the settle logic deleted. Both mutations were checked to fail
it: latching on first sight, and forgiving without counting.

That count is settled on two paths, and a unit test pins an interleaving an
ordinary fixture cannot reliably schedule: the guard reaps each child in the
same pass that reads its state, so a child dying in between is gone from
`/proc` before the next pass
and never reaches the per-pass accounting. It is forgiven where the scan ends
instead — on the kernel's ECHILD verdict, with no children left — which is
why `forgiven` takes the remaining set as an argument and is asserted directly
on a nil one.

An independent delivery canary also exercised the call site through the real
subprocess fixture. A Go overlay delayed each per-PID reap by 1.1 seconds, so
the one-second helper was observed alive and reaped by that same scan. The
settling-child test passed under `-race`; removing only the ECHILD accounting
call made it fail with `settling child not observed then forgiven`. The
overlays did not modify the committed sources.

The Git fixture gap tracked in #974 is covered at repository creation:
`gittest.InitRepo` persists `maintenance.auto=false` and `gc.auto=0` in the
fixture's common config, just as it persists identity and the signing opt-out.
Production Git commands invoked by tests (for example runtime worktree squash
and fast-forward finalization) therefore inherit the opt-outs even when they
do not use `gittest.Cmd`. Linked worktrees share that config. This changes only
test-owned repositories; production Git policy and operator repositories are
unaffected.

The regression asks real Git to resolve both keys without Cmd's flags, in the
source fixture and a linked worktree. Before the fix, all four queries reported
unset; afterward they return the opt-outs. A separate repository initialized
without `InitRepo` keeps the command-level control discriminating: both keys
are unset without Cmd and set with it. These are configuration-resolution
checks, not a timing claim about detached maintenance on the host's Git.
