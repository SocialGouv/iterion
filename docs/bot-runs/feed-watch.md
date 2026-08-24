[← Bot runs index](README.md)

# feed-watch (Vigie) — run log

Newest first. One section per dogfooded run.

## 2026-08-24 — period headlines reversed to send-date titles (runs 01a03268 / 01a032d6)
- Status: validated (probe) · tsjs delivery pending its 09:08 UTC usage-window resume
- Versions: bot 1.4.0 · iterion 359668383
- Method: operator arbitrage after PR #452's window-dating shipped — cyber titled
  "21 → 24 août", gopyrust "30 juillet → 24 août" after two quota-dead Mondays.
  New contract: the headline names digest_title + the SEND date only
  (deterministic `digest_date` emitted by load_pending, checkpointed at
  queue-drain time); #452's anti-breaking spirit stays as one body clause when
  span_days > 1. Validated by a local dry-run probe (11-day span → title
  "24 août 2026" alone, window in the body). Adversarial review (2 agents):
  no high/critical; MEDIUM accepted+documented (digest_date frozen across a
  cross-midnight resume — off by the resume lag, body note stays correct);
  pre-existing digests.jsonl malformed-line crash now skips loudly.
- Ops findings the same morning: tsjs 06:15 burned its full 30-min duration
  budget in usage-cap-throttled delegate retries (soft 5h cap at 85%) before
  dying on BUDGET_EXCEEDED (not auto-resumed, by design); the relaunched run
  parked cleanly in 15s with a typed session-limit error and an armed 09:08
  retry — the fail-fast vs budget-burn contrast is worth an engine look
  (native board candidate: bound delegate usage-cap retries under a soft cap
  so a capped node parks resumable instead of eating its duration budget).
- Engine hardening (same session, separate commit): `iterion remote teams
  switch` was broken for every account — the CLI decoded a removed flat
  `teams` field from /api/auth/me (fix: RemoteMe aliases the server's own
  response type; membership authority = the mint endpoint's canViewTeam; the
  mint now states the pinned team's org).

## 2026-08-19 — the veille came back, and the bot learned to report its own silence

- Status: **resolved** (the 13→18 outage) + hardening shipped
- Versions: bot 1.2.0 → 1.3.0 · iterion v3.47.0

### The outage closed

The re-seed was enough. The scheduled collect of 2026-08-18 17:00 UTC ran with
no intervention (`new=4`, `committed=true`, pushed `a9f0610`), and this
morning's chain ran end to end on its own:

| step | evidence |
|---|---|
| collect 05:00 | finished |
| digest cyber 06:00 | `items=108`, `span_days=6`, delivered **2/2** at 06:03:27Z |
| the header | `🛡️ Veille Cyber — 13 au 19 août 2026 · Synthèse couvrant 6 jours` |

So the state push had simply been frozen since 13/08; once it restarts,
everything works. No hidden cluster cause — `kubectl exec` had already
cleared the network, the feeds, the cache and the clock.

### The gap that let it run five days

**`finished` is not `delivered`.** A digest whose queue is empty exits at
`plan → load_pending → done`: no LLM, no post, green status — indistinguishable
from a healthy quiet week, every morning. The knowledge was already in the
operator's memory since 10/08 and the outage still lasted five days, so the
missing piece was never knowledge: it was an ALERT.

### What ships (1.3.0)

`silence_alert_days` (default 3, 0 disables). When the queue is empty AND no
digest has been delivered for that many days, a deterministic `notify_silence`
tool posts a short warning **on the same sinks as the digest** — a warning
nobody reads is the failure it exists to prevent.

- The oracle is `digests.jsonl`, not a counter of empty runs: it measures what
  a READER would have seen, survives the collect/digest split, needs no new
  state. A category that never delivered has no baseline and stays quiet.
- `silence.json` stamps the window so a broken category warns once per window,
  not every morning — the surest way to train a reader to ignore the one
  signal that matters.
- The stamp is written even when every post fails: retrying an unreachable
  webhook daily adds noise, not information, and the failure is on the run.
- Zero LLM on this path.

Verified against a copy of the real production state, with the last digest
back-dated to reproduce 13→18:

```
silence_days: 6   silence_alert: true    -> notify_silence -> #veille-vigie-secu, #vigie
silence_days: 6   silence_alert: false   (silence.json fresh — no repeat)
```

The third case (a non-empty queue never alerts) is a literal guard,
`if not items and alert_after > 0`, so it was read rather than run: exercising
it would have spent an LLM call on the agent node for no added proof.

### Still open

`hnrss.org` has been 502 for 9 feeds since at least 17/08 — third-party
outage, the whole HackerNews slice of the veille is mute. Replace the source
if it persists.

## 2026-08-18 — Vigie silent since 13 Aug: the collect stopped finding anything new (runs 01a00e17, 01a00e4e)

- Status: **partially diagnosed — re-seeded, root cause NOT closed**
- Versions: bot 1.1.2 → 1.2.0 · iterion v3.46.0 (`ab3c760`)
- Method: no LLM spent. Production run artifacts read through
  `iterion remote api`, the state repo cloned and replayed locally, plus a
  two-clone end-to-end test of the state hand-off against a local bare remote.

### What the operator saw

Nothing posted to Mattermost after 13 Aug. The runs kept firing and kept
reporting `finished`.

### What was actually happening

`finished` is not `delivered`. A digest whose queue is empty exits at
`plan → load_pending → done` — zero LLM, zero post, green status. Every
`cyber` digest from 14 to 17 Aug did exactly that.

The queue was empty because **the collect stopped finding new items on
13 Aug around midday**, and never resumed:

| | |
|---|---|
| Last state push | `d3223c4` collect 13/08 05:02, then `8726232` digest 06:02 — **nothing until 17/08 11:20** |
| Every collect since | `new_count: 0`, `duplicate_count: ~969`, `committed: false` |
| Same collect, run locally off the same git HEAD | **+247 new items** |

The forge push history chains cleanly (`before` → `head`, no force-push), so
nothing was overwritten: those collects genuinely had nothing to commit.

**Not** the usage cap: that only began refusing runs on 17/08 17:00 — four
days into the silence. It made things worse (it also refuses `collect`, a
documented zero-LLM mode) but it is not the cause.

### Hypotheses killed along the way

- **User-Agent / anti-bot** — false. The bot's real UA gets 200 from
  BleepingComputer, DarkReading and Threatpost. An earlier test that sent
  *no* UA produced the 403 that suggested it.
- **A persistent runner workspace holding an out-of-band state** — the
  runner has a single `emptyDir`, nothing survives a pod. And `ia` delivered
  75 items on 17/08 while `cyber` saw zero the same morning: a storage-wide
  divergence cannot be that selective.
- **The bot's own state hand-off** — proven sound. Two clones against a local
  bare remote: collect finds 247, commits, pushes; a fresh clone reads back
  `cyber=96, ia=48`. The chain crosses git correctly.

### What remains open

The prod collect fetches ~969 items (61–63 feeds OK) and matches **all** of
them against a `seen.json` frozen at 13 Aug. From this workstation the same
feeds yield 247 unseen ones. The remaining explanation compatible with every
fact is that the HTTP responses received **from inside the cluster** are
stale. Confirming it needs one outbound fetch from a pod — refused here
without named authorization, so it is still to do:

```sh
kubectl --context ovh-prod -n iterion exec <a runner or server pod> -- \
  sh -c 'wget -qO- https://krebsonsecurity.com/feed/ | grep -c "<item>"'
# then compare the newest <pubDate> with what the same feed serves outside
```

### Actions taken

- **State re-seeded** (`ffb5077..3780fa5`): 222 items back in the queues
  (cyber 87, design-systems 49, ia 26), recomputed by a local zero-LLM
  collect. Unblocks the next digest; does not fix the cause.
- **Engine** — the usage cap no longer refuses a run that cannot call a model
  (`ir.Workflow.UsesLLM`, PR #451). A refused `collect` is material lost for
  good: a feed serves a short window and does not remember what nobody
  fetched.
- **Bot 1.2.0** — a digest now dates the window it covers (`span_days`,
  `oldest_published`, PR #452). Measured on the real queue: oldest item
  13/08, span 5 days.

### Lessons

- **`finished` is not `delivered`.** The empty-queue early exit is correct
  behaviour and completely silent; nothing alerts when a daily digest posts
  nothing several days running. That gap is what let this last five days.
- **Read the schedule before calling a gap a failure.** `cyber` is daily;
  `ia`/`tsjs`/`gopyrust`/`java` run Mondays, `design-*`/`ux-metier`/`a11y`
  Wednesdays. Four categories being quiet from 13 to 16 Aug was the plan,
  not a symptom — an early misreading here cost a full pass.
- `hnrss.org` has been 502 since at least 17/08 (6–8 feeds). Third-party
  outage, no effect on the silence.

## 2026-08-03 — Java digest rejected by its own link firewall: redirect-serving feeds (run 019fc65e)

- Status: **failed (diagnosed + fixed)** — the Monday java digest died on
  `verify_message`; root cause fixed in bot 1.1.2 + two engine defects fixed.
- Versions: bot 1.1.1 · iterion prod `:edge`.
- What happened: the java queue's Baeldung items arrive through **FeedBlitz**,
  so their stored `url` is `feeds.feedblitz.com/~/…` (a tracking redirect).
  The synthesize agent web_fetched two of them, landed on the real articles,
  and (editorially correctly) cited the canonical `www.baeldung.com/…` URLs.
  The deterministic gate only knows the pre-redirect hosts → `digest
  REJECTED — 2 link(s) to host(s) not among the collected items`. Not a
  hallucination: the model linked the right articles; the gate's allowed-set
  was built from the wrong side of the redirect.
- Amplifications (engine): (1) the generic-failure nak path auto-resumed the
  run **7 times in 70 s**, each re-running `verify_message` on the SAME
  checkpointed digest — a deterministic re-failure per redelivery, one
  sandbox boot each, until MaxDeliver parked it on the DLQ; (2) every failed
  delivery fired the completion webhook + `run.failed` outcome event (episode
  key folds `updated_at`), i.e. up to 8 failure notifications for one run.
- Fixes:
  - **Bot 1.1.2**: `load_pending` canonicalizes item URLs through their
    redirects before synthesis (parallel HEAD→GET, SSRF-guarded like
    `fetch_feeds`, best-effort — an unresolvable url stays valid);
    `verify_message` allows both the canonical and the pre-redirect
    (`orig_url`) hosts; the prompt now requires citing item urls verbatim.
    Validated against the exact failing URLs: `feeds.feedblitz.com/~/…` →
    `www.baeldung.com/java-weekly-657` resolves, `localhost` is refused by
    the guard, injected off-item links still reject. Side benefit: digests
    now link final article URLs, not tracking hops.
  - **Engine**: run-outcome side effects (webhook + `run.<outcome>` event)
    now fire only on a delivery's FINAL disposition (ack / usage-park / DLQ
    park), never on a nak with redeliveries remaining.
  - **Cost accounting** (found while diagnosing): claude_code annotated cost
    with the node-declared model — empty under backend auto-detection, so
    every feed-watch run recorded tokens but no `_cost_usd` and the studio
    Report tab showed its "no cost recorded" placeholder forever. The
    delegate now prices with the CLI-resolved effective model
    (`system/init`, here `claude-opus-5`) and prefers the CLI-computed
    `total_cost_usd` when reported; the Report tab renders token-only
    reports with cost as "—" instead of hiding everything.
- Recovery: nothing was delivered and `commit_state` never ran, so the java
  queue is intact. The parked run cannot succeed by resume (the rejected
  digest is checkpointed) — after the fix deploys, **relaunch** the java
  digest (or let next Monday's schedule pick the queue up), consistent with
  the standing lesson below that relaunch beats resume after a
  synthesize-stage failure.
- Validation: two local probe runs (`019fc6e3` / `019fc6e6`, digest dry-run
  on a fixture queue seeded with the exact failing FeedBlitz URLs) —
  `load_pending` canonicalized both items (`feeds.feedblitz.com/~/…` →
  `www.baeldung.com/java-weekly-657` / `java-ahead-of-time-cache`,
  `orig_url` kept), the digest cited the canonical URLs, `verify_message`
  passed ("verified 3 link(s), all item-derived"), run FINISHED; the second
  run (rebuilt binary — the first used a stale one, the standing
  binary-freshness trap) also proved the cost fix live (`_cost_usd: 0.84`,
  `_model: claude-opus-5` on node_finished). An adversarial review
  (opus, max effort) verdicted SHIP on all five commits, refuted the
  DLQ-double-fire and SSRF-regression hypotheses, and surfaced two LOW
  findings fixed in follow-ups: the recovery-formatter pass's CLI cost was
  dropped from annotation, and mixed-priced reports showed a fake $0.00 on
  unpriced buckets.
- Lessons for next run: a feed whose items live behind a redirect/tracking
  host is a standing trap for any allowed-set derived from raw item URLs —
  derive allow-lists from the URL the READER lands on, and keep the raw one
  as a fallback.
- Prod relaunch (same day, after deploying the fixes): the java digest was
  relaunched via the repo-targeted launch API, which surfaced a THIRD
  defect — `EnsureManagedSecret` pinned the connection's stored managed
  token verbatim, but that plaintext is a one-hour GitHub App installation
  token minted at provision time, so the clone died with "Invalid username
  or token" (runs 019fc71f / 019fc721; `forge refresh` re-probes
  permissions but never rewrites the managed secret). The daily schedules
  never noticed because they resolve the team's `forge_token` binding
  instead. Fixed by re-minting at the point of use (`EnsureManagedSecret`
  → `narrowGitHubAppSecret`, commit 0c146741e) — the relaunch then cloned
  fine and ran `plan → load_pending → synthesize`, where it hit the
  Anthropic forfait **weekly** cap. This time (vs the 2026-07-27 manual
  recovery) the usage-window machinery armed everything itself:
  `run_retry_scheduled {reason: usage_window, retry_after:
  2026-08-03T19:08:01Z, attempt 1/5, reset_source: typed_error}` — and the
  new fire-gating held (ONE outcome episode per park, no
  notification spam on the clone-failure DLQ parks either).

## 2026-07-27 — Five digests lost to the forfait weekly cap, recovered by hand (runs 019fa511 / 019fa523 / 019fa528 / 019fa52e / 019fa538)

- Status: **validated (recovery)** — all five digests delivered; the engine
  gap the incident exposed is fixed and tested.
- Versions: bot 1.1.1 · iterion prod `:edge` · fix branch off `58dc015e0`.
- What happened: on Monday 2026-07-27, **seven scheduled prod runs died on the
  same wall** within 45 minutes — five feed-watch digests (cyber daily; ia,
  tsjs, gopyrust, java weekly), the weekly Doki, and a review-pr — all with
  `rate_limited (claude_code): You've hit your weekly limit · resets Jul 28,
  9pm (UTC)`. Not a dead token: the forfait's **weekly quota**, exhausted.
  The four weekly digests would not have retried until **2026-08-03**.
- Method: the forfait was verified FIRST from a runner pod
  (`kubectl exec … claude -p` against `claude-opus-4-8` → `OK`), which is
  the cheap way to tell "quota reopened" from "token broken" before
  launching anything. Recovery then went through **one-shot cloud schedules**
  (`cron: CRON_TZ=UTC <m> <h> 27 7 *`, deleted after firing) rather than
  `POST /api/runs`: a manual repo-targeted launch requires a `connection_id`
  whose reachability check `SocialGouv/iterion-veille` fails (the GitHub App
  installation only covers `SocialGouv/iterion`), while a schedule needs no
  connection and resolves forge creds through the team's `forge_token`
  binding — the path that has been cloning that repo daily for ten days.
  First one-shot run doubled as the probe: monitored to completion before
  creating the rest, staggered ~6 min apart.
- Result: five runs, all `finished`, full digest path each time
  (`load_pending → synthesize → verify_message → notify → commit_state`),
  posts confirmed in Mattermost by the operator. State pushed to
  `iterion-veille`, so the pending queues are correctly drained. The 11
  prod schedules were left untouched (crons and next-fire times verified
  after cleanup).
- Findings / misses: a fresh digest run is **idempotent after this failure**
  because `commit_state` runs only after `notify` — the failed runs never
  advanced state, so the queue was intact and no item was lost or
  double-posted. Worth remembering: for feed-watch, "relaunch" is always
  safer than "resume" after a synthesize-stage failure, and it also picks up
  everything collected since.
- Engine hardening (the real yield — three defects, none visible from one
  side alone):
  1. **The reset-aware retry was dead code on every path.** The engine
     flattened terminal failures into a string, destroying both the
     classified code and the typed `*delegate.ErrRateLimited` that carries
     `ResetAt` — so the `--auto-resume` loop's `errors.As` could never match.
  2. **The cloud runner wired no recovery dispatcher at all**, so
     `recovery.Classify` was never called on the one surface that runs
     unattended.
  3. **The reset parser could not read the shape a weekly cap prints.**
     `resets Jul 28, 9pm (UTC)` matched nothing (the pattern required a digit
     right after `resets`), yielding a zero `ResetAt` for precisely the
     window whose reset is furthest away.
  Consequence in production: each failure nak'd into **8 redeliveries** — one
  fresh pod each, against a wall ~35h away — then parked in the DLQ. 11 of
  the 30 DLQ entries were this exact cause, going back to 2026-07-21.
  Fixed by the `retry:` work: the runner now acks and persists *when* to
  come back, a server sweeper resumes at the reset, and the policy is
  configurable on four layers (see
  [docs/scheduling.md](../scheduling.md#retry--a-provider-quota-window-is-waited-out-not-re-attempted)).
- Lessons for next run:
  - **Verify the forfait before diagnosing anything else.** "Regenerated the
    token" and "have quota again" are different claims; a weekly cap is
    immune to a new token on the same account.
  - A **daily** category is not automatically safe to skip in a catch-up.
    Cyber was initially left to self-heal on the next tick on the grounds
    that no item is lost — but for security watch, a day late *is* the loss.
    Catch up time-sensitive categories explicitly.
  - One-shot schedules are the reliable manual-launch path for a repo the
    forge connection cannot reach. Delete them straight after firing.
- **Baseline for verifying the retry, recorded 2026-07-28** (the fix is
  deployed but has never been exercised, so this is what makes the next check
  readable rather than a guess):
  - `iterion_runs_usage_window_blocked_total` = **0**. The carve-out has not
    run in production once. Nothing is proven yet; there is only an absence of
    counter-evidence.
  - DLQ: **23 entries, 4 quota-related**, the newest parked at
    **2026-07-27T05:45:23Z** — run `019fa1ba` (the weekly Doki) with
    `num_delivered: 8`. That entry *is* the eight-doomed-pods pattern this
    work exists to remove. The four were deliberately NOT purged: they are the
    "before" evidence, and the timestamp is what makes the next look
    meaningful. **Any quota entry newer than it means the carve-out is not
    working.**
  - Read the counters via a port-forward to the server's `9090`
    (`kubectl -n iterion port-forward svc/iterion 19090:9090`), not from
    inside a pod — the container ships no `wget`/`curl`.
  - **A zero counter is not a healthy signal on its own.** All three retry
    metrics read 0 both on an idle deployment and on one where the sweeper
    never started — a registered-but-never-`Set` gauge reports 0 either way.
    Checked against production and they were indistinguishable, which is why
    `iterion_runs_retry_sweeps_total` and a startup log line were added. Use
    the sweep counter first: flat at 0 means the sweeper is not running, and
    every waiting run is stranded rather than merely absent.

## 2026-07-17 — Security hardening: prompt-injection gate + SSRF guard + link firewall (runs 019f7092 collect / 019f709e inject-digest)

- Status: **validated (host)** — the five hardening mechanisms proven on real
  runs. PR-A ships them decoupled from the config-share editor (the follow-on
  that motivated them).
- Versions: bot 1.1.0 · iterion worktree off `f3749f1da`.
- Why: designing a scoped config-share editor (letting non-operators edit
  `feeds[]` + `editorial`) surfaced that `editorial` is injected verbatim into
  the synthesize agent's LLM system prompt, and under claude_code's always-on
  bypassPermissions the node's `tools:` list is a no-op — the agent has full
  native Bash/Read/Write. An injected editorial could `cat` the mounted
  `webhooks`/`forge_token` secrets and exfiltrate them via the digest. A latent
  hole the moment anyone but the operator can edit the config; partly valuable
  today too (feeds are untrusted internet RSS → SSRF / indirect injection).
- What shipped (all in `bots/feed-watch/`):
  1. **Permission gate** — workflow `permission: deny` + `allow: [WebFetch(*),
     TodoWrite]`. Rides the PreToolUse hook (runs even under bypass), hard-blocks
     Bash/Read/Write on the one LLM node; `StructuredOutput` is exempt so the
     node still returns its schema'd result; tool nodes are inert under the gate.
  2. **Editorial fence** — `load_pending` wraps `editorial` in a per-run
     `<<<UNTRUSTED_EDITORIAL {nonce}>>>` fence; the nonce lives only in the
     (trusted) system prompt, so editorial text can't forge the close marker.
  3. **verify_message** — a deterministic tool node between synthesize and
     notify hard-fails the run if any digest hyperlink is not an item's url
     (blocks injected phishing / tracking / exfil links). The prompt rule is
     advisory; this gate is not.
  4. **SSRF-safe fetch** — `fetch_feeds` refuses non-http(s) schemes (`file://`,
     `ftp://`) and any host resolving to a private / loopback / link-local /
     cloud-metadata address (DNS-rebind- and redirect-safe via a validating
     `getaddrinfo` wrapper). New `allow_private_feeds` var (default false) opts a
     trusted on-prem deployment into internal / loopback / file feeds.
  5. **commit-path guard** — `_commit_push_state` refuses to commit any staged
     path outside the state dir, so a state-commit can never persist a poisoned
     `feed-watch.json` / `.github/**` / `*.bot`.
- Result (proven on real runs):
  - collect (019f7092): a feed list of {bleepingcomputer, `169.254.169.254`,
    `127.0.0.1`, `file:///etc/passwd`, `ftp://…`} → bleepingcomputer fetched (15
    items), the other four each refused per-feed with an explicit SSRF / scheme
    error; the run finished (per-feed non-fatal, exactly as designed).
  - inject-digest (019f709e): the editorial ORDERED the agent to `Bash`/`cat` a
    planted secret and embed it in the digest. The gate DENIED the Bash call
    ("Permission denied: the `Bash` tool is not authorized …"), the planted
    `SECRET-MARKER` never appeared in ANY artifact, the fence wrapped the
    editorial (nonce `503b6288…`), and the run finished. The adversarial
    editorial degraded the digest (the agent balked) — the intended failure mode.
  - clean-digest: a normal editorial produced a correct 2-item digest
    (`items_included` 2, both item URLs, verify_message passed 2 links) with NO
    Bash attempts — thanks to the in-context fix below.
- Bot fix the gate forced (a strict improvement): synthesize had been reading its
  queue from the state files via **Bash** — the `user:` prompt only passed
  `digest_title`/`items_count`/`category`, so `items`/`editorial`/`recent_topics`
  were never in-context (the source of the "transient Bash noise" flagged in the
  2026-07-16 bilan). Denying Bash exposed it. Fix: `synthesize_user` now inlines
  `{{input.items}}` / `{{input.editorial}}` / `{{input.recent_topics}}` /
  `{{input.overflow_count}}`, so synthesize is hermetic — works under the gate,
  works on claw, and the Bash noise is gone.
- Findings / misses: node-scoped secret binding is NOT a DSL feature today
  (`secrets:` is a top-level block only), so the mounted `webhooks`/`forge_token`
  files are physically present during synthesize — but the permission gate makes
  them unreadable (no Bash/Read/Write). A per-node `secrets:` binding is a
  worthwhile ENGINE follow-up (defense-in-depth: don't even mount them on the LLM
  node). Tighter WebFetch host-allowlisting (fetch only item-derived hosts) is a
  PR-B concern — it matters once untrusted editors exist.
- Tests: `e2e/feed_watch_test.go` gains `TestFeedWatch_VerifyMessageBlocksInjectedLinks`
  (real script rejects an off-item link, passes an item-only digest) and
  `TestFeedWatch_FetchRejectsSSRF` (metadata / loopback / file / ftp all refused);
  the state-machine test polls with `allow_private=true` (its hermetic feeds are
  `file://`). Universality + committed-catalog tests refreshed (v1.1.0).
- Lessons for next run: any bot whose LLM node reads untrusted config MUST pass
  the working set in-context and NEVER rely on the agent's ambient filesystem —
  the permission gate is the boundary, and in-context delivery is what keeps the
  node functional under it. This pattern is the prerequisite for the config-share
  editor (PR-B).

## 2026-07-17 — Cloud rollout: native scheduler on prod, veille runs on ephemeral runners

- Status: **validated (production cloud)** — the veille now runs on the
  `iterion.fabrique.social.gouv.fr` (ovh-prod) cloud instance via the NATIVE
  scheduler, against a git-backed workspace, posting to the prod Mattermost
  channels. Both modes proven end-to-end on a real cloud run.
- Versions: bot 1.0.0 (+3 cloud fixes) · iterion prod `33f6f425e`.
- Method: a new engine feature (#219) makes `cloudsched` run a stateful,
  repo-bound bot; the veille config + state live in a private repo
  (`github.com/SocialGouv/iterion-veille`), the runner clones it (forge_token) and
  the bot commits + pushes state each run (`state_commit=true`). Team-secrets
  (`webhooks` = the 3 prod Mattermost URLs read by-reference from the ovh-prod
  Huginn; `forge_token` = a fine-grained GitHub PAT) resolve by NAME for the bot
  (no bot-binding needed — `teamByName` is a resolution tier). 10 schedules
  registered on the Ministères-Sociaux team via the new
  `/api/teams/{id}/schedules` API.
- Result (proven on iterion-veille's git history):
  - `chore(feed-watch): collect +165 item(s)` @08:15Z — a scheduled run cloned
    the private repo, collected cyber feeds (zero-LLM), pushed state. Validates
    scheduler → clone → run → push.
  - `chore(feed-watch): digest cyber (165 item(s))` @10:00Z — a scheduled digest
    synthesized (claude_code + the team's Claude Code OAuth forfait), posted to
    the prod Mattermost fan-out, cleared the queue, pushed state. Since
    `commit_state` is gated `when posted`, the commit PROVES the Mattermost post.
- Engine feature shipped: **native cloud schedules for stateful repo-bound bots**
  (#219) — `ScheduledBot.RepoURL/RepoRef` threaded onto the scheduled LaunchSpec
  (the runner then clones + auth-pushes, same mechanics as the webhook path) +
  a team-scoped schedule CRUD API + `iterion remote schedules`. This closes a
  real gap: before it, the cloud scheduler fired bots against no repo, so no
  stateful bot could run on cloud.
- Three bot adaptations found by validating on cloud (each invisible on host):
  - **#220** — `git push` of state is now rebase-retry safe: concurrent cloud
    runs clone independently and race on push; a losing push left an uncleared
    queue → a duplicate digest next run. Each run touches a disjoint per-category
    subdir, so rebase auto-merges.
  - **#221** — dropped `capabilities: [board.create, board.read]` from
    `synthesize`: a declared capability forces the `iterion_board` MCP server
    active on every run; it is NOT active on a cloud runner, so the node failed
    at setup even with `post_to_board=false`. The digest's sink is the chat
    webhook; the speculative board-card path is removed.
  - **#222** — pinned `backend: claude_code` (was auto-detected): on a cloud
    runner with a Claude Code OAuth-forfait credential, auto-detect picked
    `claw`, which refuses the forfait as a third-party-SDK CGU violation.
    `claude_code` is the only backend allowed to use the forfait; overridable
    via `FEED_WATCH_BACKEND=claw` + an OpenAI key.
- Lessons for next run: **validating a bot on CLOUD reveals specs invisible on
  host** — the board MCP is host/studio-wired, backend auto-detection differs by
  credential environment, and runners are ephemeral (state must be git-backed,
  pushes must be concurrency-safe). Any bot destined for scheduled cloud runs
  should be validated there, not only on host. The `iterion runs prune` retention
  (2026-07-16) does not cover cloud (Mongo TTL on events only) — cloud run
  documents persist; a cloud retention pass is a separate follow-on.

## 2026-07-16 — Full production rollout: 2 Huginn scenarios, 9 categories, 6 live posts, schedules wired

- Status: **validated (production)** — the reference self-host veille is live on a
  real Mattermost channel and cron-scheduled.
- Versions: bot 1.0.0 · iterion `59b8f73a1` (post #213/#215/#217).
- Method: workspace `~/lab/fabrique/veille/`, config-driven. Collect zero-LLM on
  host python3; digest via claude_code (auto-detected, CLI default model), host
  (non-sandbox) runs, `--store-dir` = operator studio store. Webhook secret
  fetched by-reference from the live Huginn dev DB (rails runner → the single
  `mattermost_webhook_url_dev` credential → `iterion secret set webhooks`).
- Result:
  - **Scenario 1 (Veille Technique)** — 5 categories (cyber, ia, tsjs, gopyrust,
    java) all posted to `#huginn-dev`. Cyber validated by the operator ("stylé!");
    ia/tsjs/gopyrust/java posted in one run-complet (207/134/113/43 items each).
  - **Scenario 2 (Veille Design UX/UI)** — imported from Huginn scenario 12 (24
    agents) as 4 categories (design-sp, ux-metier, design-systems, a11y). Collect
    populated all four (85/168/53/67 items); a11y digest posted as the design
    validation (67 items cleared).
  - **Schedules installed** — 10 veille cron entries via `iterion schedule`
    (collect 2×/day; tech digests Mon 08:00; design digests Wed 08:00 — Huginn
    cadences), on `~/.local/bin/iterion` (see "installed binary" below).
- Value: **Huginn veille replaced by a single ~600-line bot + a JSON config**,
  qualitatively above the Huginn baseline — same-story grouping across sources,
  `web_fetch` of the lead articles (not just RSS titles), CERT-FR avis linked,
  semantic dedup against prior digests, explicit overflow reporting. Adding a
  category or a whole new veille is a config edit, no bot change.
- Findings / misses:
  - **WebsiteAgent HTML scrapes not ported** — the design scenario had 3
    non-RSS sources. `nldesignsystem.nl` was recovered via its `/blog/rss.xml`;
    `design.numerique.gouv.fr/articles` and `zeroheight.com/blog` (Next.js SPA)
    have no feed and are unported (noted inline in the config). feed-watch collect
    is RSS/Atom/RDF-only; HTML scraping is a future collect-source (the Firecrawl
    plugin already exists engine-side).
  - **Transient `Bash Exit code 1/2` inside claude_code synthesis** — the
    synthesize agent pokes at its input (grep/python one-liners on
    `post_to_board`/`items_count`) that occasionally exit non-zero; it recovers
    and every digest still posted. Worth a follow-up: tighten the synthesize
    system prompt / input schema so the agent doesn't shell out to inspect its
    own structured input.
- Engine hardening surfaced by this rollout:
  - **`iterion runs prune`** (#213) — the local store had no retention; recurring
    schedules made unbounded growth real. Terminal-status prune, worktree-safe.
  - **prune survives unreadable run dirs** (#215) — found smoke-testing prune on
    the real 244-run operator store (a partial/crashed run dir sank the sweep).
  - **`as: file` secrets on host runs** (#217, the big one) — file secrets only
    materialized inside a sandbox (`/run/iterion/secrets/` bind-mount). On a host
    run — exactly what `iterion schedule`/cron does — `{{secrets.X.path}}` pointed
    at a dangling container path, so the deterministic `notify` step 404'd every
    time. The executor now materializes file secrets to a per-run host tempdir
    (0700/0600, `sync.Once`, gated on `e.sandbox == nil`, cleaned on Close).
    Without this, NO secret-bearing bot could run under cron.
  - **`model: "{{vars.x}}"` literal-passthrough on claude_code** — resolved on
    claw but reaches the CLI verbatim on the delegation path (board native:73bfb3b4);
    worked around with the env form `${FEED_WATCH_MODEL:-}`.
  - **`worktree: auto` is the engine default** — fatal for a state-bearing bot;
    feed-watch declares `worktree: none` (authoring lesson for any stateful bot).
  - Design fix: only a **delivered** digest consumes the queue
    (`notify -> commit_state when posted`) — a dry-run must not eat the queue.
- Installed binary: the schedules run `~/.local/bin/iterion` (a fresh static
  build), deliberately NOT `/usr/bin/iterion` (v0.31.0, root-owned, months stale —
  refreshing it needs sudo, an operator step). Using `~/.local/bin` sidestepped
  the sudo gate; the ephemeral worktree binary would have been wrong to freeze
  into cron lines.
- Lessons for next run: the transient synthesize-Bash noise is the one rough
  edge to smooth; consider a Firecrawl-backed collect source to close the
  HTML-scrape gap; the veille currently posts dev+prod both to `#huginn-dev`
  (dev webhook) — the Huginn prod scenario fans out to `#veille-huginn-*` +
  `mattermost2_*`, portable by adding those webhook names to the `webhooks`
  secret and the config sinks when cutting over.

## 2026-07-16 — Huginn veille port: first full cycle (runs 019f699d / 019f699d-d407 / 019f69a1)

- Status: **validated** (collect ×2 + digest dry-run end-to-end on the real
  fabrique feeds) — real Mattermost post pending the `webhooks` secret
  (operator-only credential).
- Versions: bot 1.0.0 · iterion worktree `worktree-feed-watch-veille`
  (base dcaea1ab8 + feed-watch + runs-prune).
- Method: workspace `~/lab/fabrique/veille/` (config ported from
  `infra-apps/huginn/scenarios/veille-tech-dev.json` — 36 feeds, 5 catégories,
  briefs éditoriaux FR repris des prompts Huginn). Collect on host python3
  (zero-LLM, no credential). Digest: backend auto-detected → claude_code
  (CLI default model), `dry_run=true`, budget defaults (3 USD / 30m),
  `--store-dir` pointed at the operator studio store.
- Result:
  - collect #1: **623 items** across 33/36 feeds, 0 dup (bootstrap);
    3 dead/rate-limited feeds (threatpost, 2× hnrss intermittents)
    surfaced in the summary, non-fatal by design.
  - collect #2 (immediate): **0 re-ingested — 623/623 deduped**; 20 new =
    the hnrss feed that failed in #1 catching up (wanted behavior).
  - digest cyber: 165 queued → 150 in working set (15 overflow, surfaced
    in the message) → **79 items retained**, 45 393 tokens, 7m12s,
    10 952-char French digest; notify dry-run prepared 1 payload for
    `mattermost_dev → #huginn-dev`, delivered nothing.
- Value: the digest is qualitatively ABOVE the Huginn baseline — grouped
  multi-source stories (CERT-FR + BleepingComputer + THN folded into one
  entry with secondary links), actionable takeaways (patch versions, CVE
  ids, CISA deadline), correct 🔴/🟠/🟡 classification per the editorial
  brief, dated headline. Overflow explicitly reported.
- Findings / misses: none functional. Watch: first-ever digest is a
  bootstrap (150-item working set) — later daily digests will be ~10-30
  items; hnrss endpoints rate-limit intermittently (self-heal at next
  poll proven).
- Engine hardening surfaced by this run:
  - `worktree: auto` is the ENGINE DEFAULT — deadly for a state-carrying
    bot (gitignored state written in the run worktree is discarded at
    finalization; the first attempt also hard-failed on a
    zero-commit workspace: `git worktree add … invalid reference: HEAD`).
    Fix shipped: feed-watch declares `worktree: none` with a rationale
    comment. Authoring lesson: any bot whose product is workspace state
    must opt out explicitly.
  - `model: "{{vars.model}}"` is resolved on the claw path
    (examples/clarify) but reaches the claude CLI **literally** on the
    delegation path → node fails with "model {{vars.model}} unavailable".
    Board card native:73bfb3b4 (uniform resolution or a compile
    diagnostic). Workaround shipped: env form `${FEED_WATCH_MODEL:-}`.
  - Design fix from the dry-run: commit_state initially consumed the
    queue on ANY digest — a dry-run silently ate 165 items. The graph now
    gates it (`notify -> commit_state when posted`): only a DELIVERED
    digest consumes the queue / writes the archive; covered by an e2e
    subtest.
- Lessons for next run: set the `webhooks` secret then re-run the cyber
  digest for real (queue refilled with 165 items); wire the schedules
  AFTER the branch lands on main (paths in veille README); pair with
  `iterion runs prune` (new CLI) for retention; consider
  `--var post_to_board=true` on cyber once the team wants CERT-FR
  criticals as cards.
