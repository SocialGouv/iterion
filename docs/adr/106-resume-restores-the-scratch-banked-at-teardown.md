# ADR-106 — A resume restores the scratch its run banked at teardown

- **Status**: Accepted
- **Date**: 2026-09-30
- **Applies to**: `pkg/runtime` (sandbox teardown, every resume path), `pkg/store` (`ScratchBankStore`)
- **Neighbours**: ADR-082 (sandbox write-back of the workspace), ADR-089 (packed CLI sessions persisted for resume)

## Context

Nodes keep working state under `${PROJECT_SCRATCH_DIR}`: a `verify.sh` an
agent writes and a later gate executes (eleven catalog bots), a measurement a
later node reads, source clones kept out of the judged tree. In a sandbox the
scratch is `/tmp/iterion-scratch`, bound to a host directory only when the
driver has host bind mounts, host state is on and the UIDs match.

On kubernetes none of that holds, so the scratch lives in the container and
dies with it. A resume always starts a new pod. At park, only the workspace
travels, and only as commits. Measured: an assessment run wrote its floor
measurement, parked, resumed on a new pod, and died `MEASUREMENT_REFUSED` on
the file it had written. A docs run lost its source clone and its agent
rebuilt it. The cloud runner's in-pod mode loses the scratch across runner
pods the same way.

## Options

- **(a) Bank the scratch.** The teardown streams it into a per-run bank; the
  resume restores it before its first node. This covers every writer,
  agents included, with no language change. Its costs are storage per parked
  run and a transfer at each park and resume. The largest measured scratch, a
  source clone, is about 35 MB before compression.
- **(b) Replay declared producers.** A DSL attribute would mark rebuildable
  tool nodes, which the resume would re-run. It misses the agent writers. It
  also re-runs them on inputs rebuilt from the resumed state, because the
  checkpoint records neither the order nor the inputs of the nodes that ran.
  And it touches about twelve sites of the language.
- **(c) A named refusal alone.** This is honest but saves no run.

## Decision

**(a), bounded.**

- **Teardown.** A sandbox whose scratch is not host-backed banks the scratch
  unless the run finished: a park, a failure, and a `failed` run — which a
  rewind brings back — all may resume. The gzip'd tar goes through a host
  temporary file, so the cap is enforced before anything is uploaded; it is
  then stored under the run (`ScratchBankStore`: `sessions/<run>/scratch.tgz`
  in the cloud, `runs/<id>/scratch-bank.tgz` on the filesystem, swept with
  the run). An empty scratch drops a previous bank; a finished run keeps
  none. The outcome is an event,
  `sandbox_scratch_banked {banked, empty, bytes, reason}`. Both are written
  under the run's identity and past its cancellation: a drain, a lost lease
  or an operator's cancel reaches the teardown first.
- **Cap.** 256 MiB compressed. Past it, or on a listing, tar or upload
  failure, or in a store that keeps no bank, nothing is stored and the event
  names why.
- **Resume, before the claim.** A run whose last teardown recorded a
  non-empty scratch it could not bank is refused `SCRATCH_NOT_PORTABLE` (a
  deterministic failure code). So is a run that moved past its bank: a node
  finished in a sandbox started after the last `sandbox_scratch_banked`,
  which only a sandbox lost without a teardown leaves behind — restored, the
  bank would revert what that node wrote. A node a resume finishes before
  its sandbox starts (the answered human node) does not count. `--force` resumes either as it stands, and says so. A
  timeline that cannot be read refuses the resume too: it never reads as
  "nothing recorded".
- **Resume, after the new sandbox starts** (the pause family and the failure
  path alike). The bank is extracted before the first node
  (`sandbox_scratch_restored {restored, bytes, stale, forced, reason}`). Every
  failure holds the bank: the failed sandbox's teardown banks nothing over it.
  A bank that is gone or does not extract parks the run
  `SCRATCH_NOT_PORTABLE`, and `--force` goes on without it. A read that fails
  on the way — the timeline, the store, the stream — parks it without a
  code: the runner redelivers, and `--force` does not skip it.

## Consequences

- A parked run on kubernetes resumes with its scratch. The eleven
  verify-gate bots, the assessment bot and the docs bot no longer depend on
  never parking.
- Storage grows by one object per parked or failed run, bounded, and removed
  with the run.
- The refusal turns a late, misattributed failure into an immediate one that
  names its cause. Relaunching the run fresh is the remedy.
- **Not covered: the host scratch of an unsandboxed cloud runner.** It is
  lost across runner pods and is not banked, because the engine cannot tell
  a same-host resume from a cross-pod one there. It is the next step if that
  mode carries long runs.
- **Scratch as cross-run memory** (a ledger kept between runs of one bot) is
  still not portable on kubernetes. This decision banks one run's scratch
  for that run.
