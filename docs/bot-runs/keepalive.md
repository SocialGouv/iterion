# Bilans — always-on (`overlap: keepalive`) + `examples/keepalive`

The keepalive/always-on scheduling feature and its demo bot
(`examples/keepalive/main.bot`, a tool-only heartbeat). See
[docs/scheduling.md#always-on-agents--overlap-keepalive](../scheduling.md).

## 2026-07-20 — feature dogfood: always-on end-to-end (local studio)
- Status: validated
- Versions: feature branch `feat/keepalive-always-on` · iterion @ worktree head (post c31a52f)
- Method: `iterion studio --store-dir "$PWD/.iterion" --port 7801` with
  `ITERION_SCHEDULER_INTERVAL=3s`; two keepalive bots auto-seeded from
  manifests — `heartbeat` (tool-only, instant, `interval:15s`
  `stale_after:2m`) and a throwaway `slowbeat` (`sleep 60`, `interval:15s`
  `stale_after:10s`). No API keys (tool-only). In-process `trigger.Scheduler`.
- Result: converged behavior on all three keepalive invariants —
  - **Relaunch loop**: `heartbeat` produced a stream of fresh `finished`
    runs at ~15–20s spacing (7 over ~2min), all stamped
    `source.kind=schedule` + same `schedule_id`.
  - **At-most-one-live**: `slowbeat` (60s runs) kept exactly **one** live at
    a time — no stacking.
  - **Staleness + reap**: a `slowbeat` run silent past `stale_after=10s` was
    CAS-flipped `running → failed_resumable` and a fresh one relaunched, on
    every tick — log: `scheduler: … (slowbeat) reaping 1 stale keepalive
    run(s) […]` + `server: keepalive reaped stale run …`.
- Value: proved the whole local path (studio → in-process scheduler →
  schedgate keepalive gate → runview launcher → run store → reaper) works
  as designed: an agent stays continuously alive as a stream of short,
  individually-budgeted runs, self-recovering from a stuck run within one
  tick — without fighting `max_duration`/GC.
- Findings / misses: the FIRST live attempt showed `slowbeat` **stacking**
  (3 `running` at once, no reap) — surfaced a real wiring bug, fixed below.
- Engine hardening (bugs found → fixes, same branch):
  1. **Local scheduler ran gate-less.** `Server.scheduleGate()` returned nil
     whenever `cfg.Store == nil`, which is always true in local/studio mode
     (runview owns the store). So the in-process keepalive scheduler had no
     overlap/staleness/reap gate and every tick fired unconditionally. Fix:
     fall back to `s.runs.RunStore()` when `cfg.Store` is nil
     (`pkg/server/trigger_coordinator.go`). This is what made at-most-one-live
     + reaping actually engage.
  2. **Keepalive subs weren't auto-loaded.** `buildLocalTriggerStore`
     (`pkg/cli/trigger.go`) seeded board + schedule invocations but not
     keepalive, so a `kind: keepalive` bot never activated out of the box.
     Fix: seed `FromKeepaliveInvocation` too.
  3. **Bundle pairing needs `main.bot`.** The demo's `heartbeat.bot` was
     read as a loose file (manifest ignored → `invocations: null`); renamed
     to `main.bot` so the manifest pairs and the keepalive invocation loads.
- Lessons for next run:
  - Sub-minute keepalive **requires the resident scheduler** (studio/server);
    host crontab floors at 1 minute. `ITERION_SCHEDULER_INTERVAL` sets the
    in-process tick resolution (default 15s).
  - Scheduled runs create a worktree per run even for tool-only bots (a
    launcher default, not keepalive-specific) — noted, not blocking; watch
    disk under a fast always-on cadence.
  - Test staleness with a **separate store dir / fresh process** — `rm -rf
    .iterion/runs` while a prior studio's runs are in flight triggers a drain
    and "run not found" noise. Kill all `iterion studio` procs first.
