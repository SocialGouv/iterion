# ADR-118 — Merge-gate settle marks, and the verdict attributed at publish time

- Status: accepted
- Date: 2026-09-30
- Deciders: jo (direction: "the most powerful, flexible, lasting and reliable" of the options below), Claude (analysis)
- Relates to: [117-server-side-leader-lease-for-idempotent-nets.md](117-server-side-leader-lease-for-idempotent-nets.md)
  (the sweep's election — this record is what the elected sweep still spends); issue #2002; [docs/merge-gate.md](../merge-gate.md)

## Context

The merge-gate sweep (`pkg/server/forge_gate_sweeper.go`) re-offers every
gating run in its window to the reconciler: every minute for an hour, then
every deep pass for 192 hours. Each offer that gets past the grant costs a
forge read (`GET pulls/{n}`, often `GET commits/{sha}/status`). Nothing
remembered a run the reconciler had already found settled.

Measured on SocialGouv/iterion's eight-day window on 2026-09-30 (#1995): 484
gating runs, 416 on merged pull requests and 46 on closed ones — 95 % settled.
Each run cost about 440–880 reads from ONE sweeping replica over its grant's
life (60 fast passes, 384 deep ones, one or two reads each), against 10–15 for
the run's own work — and every replica swept. Election (ADR-117) divides that
by the replica count; it does not stop the reads.

The code carried a dormant arm meant to "give the grant back" once the run's
own verdict was posted, which never fired. Ownership was read from the
status's target URL, and no real verdict carries the run there: its target is
the review, where every reviewer lands from the pull request's check. Making
the check point at the run would have moved every reviewer's landing — a
product decision the code refused to take on its own.

## Options considered

- **A.** Settle marks in the shared store; leave the grant and the dormant arm
  as they are.
- **B.** A, plus attribute the verdict at publish time, which repairs the
  dormant arm without moving anyone's landing. **Chosen.**
- **C.** Revoke the grant as the forgetting mechanism, the check pointing at
  the run. It moves every reviewer to the studio, breaks the auto-fix lane on
  a red verdict and resumes, and still covers neither merged nor closed runs.
- **D.** A settled field on the run document in Mongo. It is durable past a
  Valkey flush, but the settle state is only useful while the grant lives, and
  the grant lives in Valkey. The write must also not move `updated_at` (the
  notification episode key — a moved one re-notifies and re-alerts), and it
  needs a partial index, a filesystem twin, and a change to a query three
  consumers share.

## Decision

1. **Settle marks.** When the reconciler finds a run settled, it writes a mark
   keyed by run id: `{reason, sha, episode}`.
   - The episode is the run's `updated_at` in milliseconds, so a resumed run is
     looked at again.
   - Permanent reasons hold for the rest of the run's horizon: `merged`,
     `verdict_success`, `verdict_failure`, `unpinned` (no reviewed revision).
   - Reversible reasons are re-read after 6 h: `closed` (a pull request can
     reopen, and a reopen on the same head launches no fresh review) and
     `head_moved` (a force-push can come back). A reversible mark ends one deep
     interval before the run leaves the window, so its re-read happens inside
     it; closer to the edge, none is written.
   - The sweep reads a page's marks in one round-trip, before offering either
     lane. A `verdict_failure` run is still offered to the auto-fix lane, whose
     trigger it is.
   - A deep traversal the page budget cuts short (2 000 rows a pass) continues
     at the very next pass, beside that pass's fast one, until the window is
     exhausted; only a page that fails twice in a row parks it until the next
     deep pass. A reversible mark's last re-read, and the "every run sees two
     deep passes" bound below, both count on a traversal ending within a deep
     interval.
   - Store: Valkey `iterion:gate:settled:<run>` with a TTL, or an in-memory twin
     when the deployment has no Valkey — the same split as the publish grant.
   - A lost mark, or a store that cannot answer, costs a re-read: the sweep
     then offers every run, and says so once.
2. **The verdict, attributed at publish time.** The publish endpoint records the
   verdict it posted on the grant that posted it: `verdict {sha, context, state,
   at}`.
   - The write is `WATCH` + `SET … XX KEEPTTL`, so it never re-creates an
     expired or revoked grant and never pushes its expiry out. A write that
     fails clears the earlier record rather than leave it standing.
   - The endpoint knows the grant, not the run. The reconciler therefore trusts
     the record only on a grant no second run publishes with (not `shared`):
     for its reviewed head and its pinned check, the run is settled on the
     event path, with no forge read. On a shared grant it reads the forge.
3. **The repaired arm is a cut-back, not a revoke.** The grant drops to the
   ordinary post-run grace (lookback + 30 min) instead of living out the
   192 h 30 min gate grace, once nothing will post with it:
   - after the run's own green verdict;
   - after its own red verdict, when the repo's auto-fix lane is off or the
     repo has no integration — with the lane on, or its store unreadable, the
     grant stays: the lane reads it to launch a fixer;
   - when the reconciler finds the pull request merged — which it reads only
     for a run not already settled on a verdict.

   The grant reaper's ordinary grace — the end of a run that gates nothing, or
   names no reviewed revision — is the same cut-back. No grant is cut back
   under a second run, by a verdict or by an end: a shared grant keeps its TTL,
   or the gate grace once a gating run that names its revision ends. Nor under
   a run that is not over — resumed after the sweep listed it, or by the time a
   late outcome event reaches the reaper.
   `shared` and `cut_back` are set in ONE update of the grant, so a pinned
   launch and a cut-back racing on it cannot both win: the launch marks it
   shared first and it is kept, or it finds it cut back and is refused, like a
   grant that cannot be minted. A fork (which inherits the token) shares it the
   same way; a fork of a grant already cut back, or already gone, is warned
   about.
4. **The unpinned exit is free.** A run naming no reviewed revision is settled
   before any forge read, and the grant reaper gives it the ordinary grace: no
   repair can speak for it.
5. **The cadence is an operator's lever.** `ITERION_GATE_SWEEP_INTERVAL`,
   `_LOOKBACK` and `_DEEP_EVERY`:
   - A value that does not parse keeps its default and warns, naming the
     variable, as does an interval under 1 s or of half the horizon or more, a
     lookback not exceeding interval + the 3-min grace or reaching the horizon,
     and deep passes half the horizon apart or more — checked by division, so a
     huge pass count cannot overflow into an accepted one.
   - The horizon stays derived from the grant.
   - The ordinary post-run grace follows the configured lookback.
   - The sweep's lease is paced by the interval, capped at the default minute,
     so a long interval does not also stretch the failover; a term's first pass
     runs at once.

## Consequences

- A gating run settled green costs the sweep nothing past the minute it ended.
  A run on a merged pull request costs one read. A red verdict stays offered
  to the auto-fix lane, which reads the pull request until it launches a fix —
  every pass, for the horizon, on a pull request the lane refuses (a fork's).
  Modelled at the 30/09 volume, the sweep drops from ~1.2k–1.9k reads/h
  (elected) to a few dozen.
- A forge-write bearer no longer outlives its use by eight days in the common
  case. Two kinds still keep the gate grace: a run settled on a verdict found
  only on the forge (another run's — it names nothing this run's grant is
  still for), and a red verdict on a repo whose auto-fix lane is on. Neither is
  read again, so a later merge does not shorten them.
- **Assumed:** resuming a gating run more than lookback + 30 min after its
  grant was cut back answers 401 at publish, as an ordinary run already does.
- **Assumed:** turning a repo's auto-fix lane on reaches the red verdicts
  posted after; those already cut back have no grant left for a fixer.
- **Assumed:** a reopened pull request, or a head force-pushed back, whose
  event launches no fresh review, is repaired within 6 h instead of 30 min.
- **During the rollout**, old server pods read the new grant fields as unknown
  JSON and ignore them, and never lengthen a grant (every shortening is
  `EXPIRE … LT`). Their publishes record no verdict: a grant with no record
  sends a new pod to the forge, but a record a new pod wrote earlier stays —
  a run published twice across the two builds can be settled on its first
  verdict, and a red second one then misses its auto-fix offer. Nor do they
  mark a pinned or forked grant shared: a launch or fork one of them serves can
  have its grant cut back under it by a new pod, and that run's publish then
  answers 401 once the grace runs out. The exposure ends when the last grant
  an old pod minted or left unshared expires — its TTL, up to nine days — not
  with the rollout itself.
