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
  `sandbox_scratch_banked {banked, empty, unknown, bytes, reason}`. Both are
  written under the run's identity and past its cancellation (a drain, a lost
  lease or an operator's cancel reaches the teardown first). A failure
  another try may cure — a blip on the exec, a failed tar — is tried again,
  and so is the record; a later try never replaces what an earlier one saw
  with less (files, then empty, then nothing); an upload is tried again on
  the same archive. Before tar reads the scratch, every other process of
  the sandbox is stopped (`kill -STOP -1`: the export ran, the sandbox dies
  next), so nothing writes what it reads — a write tar reports nothing of,
  a file moved between two directories, a page written through a shared
  mapping, would tear the archive in silence. Only a sandbox whose commands
  run in a process namespace of their own is stopped
  (`sandbox.ProcessIsolated`): a kubernetes pod, and a container whose
  process namespace, read when it started, is its own — a runtime default
  such as podman's `pidns = "host"` rules it out. A quiesce that fails is
  recorded (`unquiesced`); once the archive is taken the processes go on,
  so the sandbox's shutdown is not held. tar runs untranslated (`LC_ALL=C`). A member it catches
  changing or vanishing while it reads it (its warnings, with the line
  `kubectl exec` adds) leaves an archive that may hold no state the scratch
  was ever in — a file torn between two writes, a file renamed into place
  missing — so tar runs again while it races, and while the budget allows
  another archive and its upload. The last complete archive — a clean one,
  or else the last that raced — is banked even when a later try fails or
  runs out of time, with the raced members named (`raced`), and its restore
  says so. Its notice for a socket, which no archive holds, is neutral. A
  scratch that is a link to a directory is listed through the link. A
  scratch the teardown cannot even list — the
  sandbox is already gone — is `unknown`: nobody knows whether it held
  anything. It does not replace a bank recorded before it: the lost sandbox
  started from that bank, which is still stored.
- **Cap.** 256 MiB compressed. Past it, or on a tar or upload failure, or in
  a store that keeps no bank, nothing is stored and the event names why.
- **Subbot children.** A child that can park (a human gate, an interactive
  node, a permission gate that asks) is refused adoption into its parent's sandbox when that sandbox's
  scratch lives in the container, as under a copy-based parent: parked, the
  child is resumed on its own, in a sandbox without that scratch. A child
  adopted there that parks anyway — a recovery pause, an operator's pause,
  a cost cap — records it (`sandbox_shared {scratch_container_local}`), and
  its own resume is refused `SCRATCH_NOT_PORTABLE`; `--force` goes on
  without the scratch.
- **Resume, before anything moves the run.** A run whose last teardown
  recorded a non-empty scratch it could not bank is refused
  `SCRATCH_NOT_PORTABLE` (a deterministic failure code). So is a run that
  moved past its bank: a node that runs in the sandbox — an agent, a judge,
  a tool, a subbot, an LLM router — finished after the bank was recorded,
  which only a sandbox lost without a teardown leaves behind: restored, the
  bank would revert what that node wrote. Only a node that succeeded counts:
  one that failed re-runs from the checkpoint. The engine-side kinds (a
  human node, a condition router, a compute) never wrote there, nor did a
  paused node — a human node or an agent that asked — that a resume records
  as finished by its answer (`node_finished {answered}`). A rewind takes
  back the nodes it dropped that lie on no cycle: one in a loop or a foreach
  may have run passes the rewind does not replay. A resume that went on
  without the bank
  (`--force` past a bank that is gone) forsakes it: nothing is restored or
  refused over it later; one that restored a stale bank with `--force` makes
  it the run's scratch again. The
  resume surface refuses from a record the run's latest execution wrote,
  before a cloud resume is flipped to `queued`. A latest execution that wrote
  none may still be banking — the run already reads paused or failed while
  its teardown runs — so the surface leaves it to the engine, which checks
  under the run's lock, before its claim. `--force` resumes as it stands,
  and says so. An `unknown` record is not refused; after a bank, the bank
  still decides. A timeline that cannot be read refuses the resume: it never
  reads as "nothing recorded".
- **Resume, after the new sandbox starts** (the pause family and the failure
  path alike). The bank is extracted before the first node
  (`sandbox_scratch_restored {restored, bytes, stale, forced, reason}`),
  read onto the host and checked to extract first: a failure in the sandbox
  can then only be a transport's. Every failure holds the bank: the failed
  sandbox's teardown banks nothing over it (the hold ends with that
  sandbox). A bank that is gone or does not
  extract, or a resume that runs without a sandbox, parks the run
  `SCRATCH_NOT_PORTABLE`, and `--force` goes on without it. A read that fails
  on the way — the timeline, the store, the stream into the sandbox — parks
  it without a code: the runner redelivers, and `--force` does not skip it.

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
- **Not covered: what a sandbox lost without a teardown wrote after an empty
  or unknown record.** The resume goes on without it, as before banking
  existed. Refusing would stop every hard-killed run, most of which never
  write the scratch.
- **The engine's check holds under the run's lock** only if the lock is held
  through a cancelled run's teardown: the runner keeps refreshing the lease
  until the engine returns (#1991). A pod whose lease lapsed while it was
  alive still unwinds, and banks, as a split-brain writer.
- **Staleness is read from the timeline's `node_finished` events.** A branch
  writes its node's finish best-effort: one lost to a store outage leaves a
  bank looking fresh. Stamping the record with the checkpoint's progress,
  and comparing it with the checkpoint a resume runs from, would read
  staleness from the fact itself — the next step if that case is met.
- **Not covered: a write while tar reads, in a sandbox that is not process
  isolated or whose quiesce failed** (`unquiesced` on the record). Neither
  GNU tar nor busybox tar reports every such write.
- **A rewind across a loop or a fan-out is refused.** A rewind takes back
  the nodes it dropped that lie on no cycle; a node in a loop or a foreach
  may have run passes it does not replay, and a fan-out's in-flight
  branches are not among the nodes it names. Such a resume is refused
  though the bank may be exact, and `--force` resumes it on that bank.
  Reading the rewind's replay from the timeline is the next step.
- **The scratch is not rewound.** A rewind to a node that ran before the
  bank replays it on what it wrote there, unlike the workspace's files.
- **Not covered: sparse files.** A sparse file is banked compressed and
  restored dense (tar runs without `--sparse`, which busybox tar lacks).
