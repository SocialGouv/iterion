# Philosophy

[Why Iterion?](why-iterion.md) explains what the engine is for. This page
explains **how we decide** when a design question is open — the stance the
tactical rules in `CLAUDE.md` serve, and the two arbitrations that are already
settled so they don't get re-opened on every feature.

| Pillar | One line |
|---|---|
| **1. Maximum power** | Remove ceilings the operator did not choose; a load-bearing limit keeps an explicit escape hatch. |
| **2. Modularity** | A new capability implements an existing seam, or is a declarative artifact — never one more branch in the core. |
| **3. Cloud-native by construction** | HA and horizontal scale are designed in from the first commit, never ported in later. |
| **4. Git-native is first class** *(pre-arbitrated)* | The forge/factory features are not legacy to hide under a product layer. |
| **5. Product-oriented, additively** *(pre-arbitrated)* | Non-dev surfaces and non-code use cases are welcome — as projections, not replacements. |

---

## 1. Maximum power, no artificial limitation

Iterion is an engine for people who want to push agent work further than the
defaults. Its job is to remove ceilings the operator did not choose. Anything
the engine reads internally should be reachable from outside, through the
precedence chain the codebase already uses everywhere:

```
CLI flag  →  node field  →  workflow block  →  ITERION_* env  →  package default
```

That chain is not decoration — it is how `compress:`, `permission:`,
`auto_memory:`, the sandbox defaults and the retry policy are all resolved.
A new dial that skips it is the exception that has to argue for itself.

### The test: artificial vs load-bearing

A limit is **artificial** when it exists only because nobody wired an
override, or out of taste, or because a closed enum was easier to type than an
open list. Lift it.

A limit is **load-bearing** when removing it breaks a guarantee the product
sells:

| Guarantee | What enforces it |
|---|---|
| Convergence / asymptote | bounded `as name(N)` loops, termination contracts, [`iterion bench asymptote`](asymptote-bench.md) |
| Budget safety | shared token/cost/duration tracking, `max_cost_usd`, the per-org launch gate ([quotas](quotas-and-limits.md)) |
| Determinism of gates | a gate is a `tool`/`compute` node reading a real exit code — never an LLM judgment |
| Workspace safety | one mutating branch at a time; `WORKSPACE_SAFETY` runtime error |
| Tenant isolation | every store keys on `Team.ID`; sealed per-run credential bundles |
| Secret handling | [`pkg/secrets`](../pkg/secrets/) sealing; secrets travel by reference |
| Explicit errors | no silent fallback that masks a failure |

**Load-bearing limits stay — and carry an explicit, greppable escape hatch.**
That is the whole difference between a paternalistic engine and a powerful
one. The shipped hatches are the pattern to imitate:

| Guardrail | Escape hatch | Where |
|---|---|---|
| The `.bot` graph is statically terminating | `as name(unbounded [<fuel>])` — Turing-completeness by typing one keyword | [DSL totality & TC](dsl-totality-and-tc.md), [ADR-050](adr/050-dsl-turing-completeness-fuel-liveness.md) |
| Runs are sandboxed by default | `sandbox: none` — diagnostic **C128** *warns*, it does not forbid | [sandbox](sandbox.md), [ADR-082](adr/082-sandbox-by-default.md) |
| Tool calls can be gated | `permission: off` **is the default**; the gate is opt-in | [permissions](permissions.md) |
| Resume refuses a changed source | `iterion resume --force` | [resume](resume.md) |
| A bot declares its own budget | `--max-cost-usd` / `--max-tokens` / `--max-duration` / `--max-iterations` / `--max-parallel-branches` re-budget **any** bot without editing its `.bot` | [CLI reference](cli-reference.md) |
| The typed remote CLI covers N endpoints | `iterion remote api <METHOD> <path>` reaches all of them | [cloud CLI](cloud-cli.md) |
| The MCP server exposes curated tools | the `remote_api` tool is the raw passthrough | [MCP server](mcp-server.md) |
| Host state is auto-mounted | `--sandbox-host-state=none`, `ITERION_SANDBOX_HOST_STATE` | [sandbox](sandbox.md) |

### Corollaries for contributors

- **A hardcoded constant that bounds user work, with no override, is a
  defect.** Not a style preference — a defect, reportable as one.
- **Prefer to warn over to reject.** When an operator could legitimately want
  the thing, emit a `C1xx` diagnostic and let them proceed. `C128`
  (`sandbox: none`) and `C111` (permission rules without a gate) are the model;
  a hard compile error is for what is genuinely incoherent.
- **Never silently replace an operator's explicit choice.** The `auto_memory`
  precedent: the run-level override travels all the way onto the cloud queue
  (`RunMessage.auto_memory`) and into detached subprocesses, precisely so a
  bot's `on` cannot overwrite an operator's `off`. Precedence that only holds
  in-process is precedence that lies.
- **A closed enum that fences out use cases is the smell.** Languages,
  providers, trackers, ecosystems: emit an open list. This is already enforced
  for catalog bots by `bots/catalog_universality_test.go`; the same instinct
  applies engine-side.
- **Power is not permission to be unsafe by default.** The default can be the
  careful one (sandbox on, `auto_memory` off, mono review topology) — what
  matters is that the operator can always reach past it, in one documented
  move.

---

## 2. Modularity and extensibility are central

Iterion grows by **adding implementations of existing seams**, not by growing
the core. Anything else compounds: a `switch` arm today is three `switch` arms
and a migration next quarter.

| Seam | Interface | Shipped implementations |
|---|---|---|
| Node execution | `NodeExecutor` ([engine.go](../pkg/runtime/engine.go)) | `ClawExecutor`, test stubs |
| LLM/agent backend | `delegate.Backend` + `SystemPromptModeForBackend` ([delegate.go](../pkg/backend/delegate/delegate.go)) | claw, claude_code, codex, pi, kimi, grok — [ADR-065](adr/065-dedicated-cli-agent-backend.md), [backends](backends.md) |
| Issue tracker | `tracker.Tracker` ([tracker.go](../pkg/dispatcher/tracker/tracker.go)) | native kanban, GitHub, Forgejo |
| Git forge | [`pkg/forge`](../pkg/forge/) provider adapters | GitHub, GitLab, Forgejo — [ADR-049](adr/049-forge-as-interchangeable-substrate.md) |
| Shared memory | `knowledge.MemoryStore` ([pkg/knowledge](../pkg/knowledge/)) | filesystem, Mongo — [memory](memory-and-knowledge.md) |
| Secret sealing | `secrets.Sealer` ([pkg/secrets](../pkg/secrets/)) | AES-256-GCM; the seam a KMS backend plugs into |
| Event transport | `eventbus.Bus` ([pkg/eventbus](../pkg/eventbus/)) | `InProcBus`, `NATSBus` |
| Run triggers | [`pkg/trigger`](../pkg/trigger/) sources | board, run-completion, schedule, git-forge |
| User notification | `usernotify.Sink` ([pkg/usernotify](../pkg/usernotify/)) | Web Push; desktop + email on the same interface — [notifications](notifications.md) |
| Command-output compression | the rewriter chain | rtk, and any plugin declaring `rewriters` |
| Everything declarative | [`pkg/plugin`](../pkg/plugin/) `contributes:` kinds | rewriters, MCP servers, skills/commands/agents, hooks, lifecycle — [plugins](plugins.md) |
| Skills | [`pkg/skilllib`](../pkg/skilllib/) + bundle skills | [ADR-059](adr/059-skill-library.md), [ADR-079](adr/079-cloud-portable-plugin-and-library-skills.md) |
| Board operations | [`boardops`](../pkg/dispatcher/native/boardops/ops.go) | stdio MCP, HTTP MCP, in-process claw tools — one package, three transports |

### The Nth-variant test

> If adding the **next** variant costs an engine PR, an `if` arm, or a new
> schema enum value — the seam is missing.

Build the seam at the **second** variant, not the fifth. `boardops` is the
reference: three transports, one implementation, identical validation and
event semantics on all three.

Two rules elsewhere in the repo are this pillar applied, and are worth reading
as such:

- **The engine stays bot-agnostic.** A new bot is a bundle, never an engine
  PR. Run-to-run hand-off is declared with `produces:` / `consumes:` and
  matched **by kind** (`review`, `review_ledger`), so neither manifest names
  the other bot. Adding a second reviewer is a bundle.
- **Stack knowledge lives in skills.** A universal code bot adds a language by
  dropping `skills/lang-<id>.md` — zero DSL edits, with a deterministic gate
  verifying the coverage actually happened.

And the constraint that keeps all of this cheap: **the plugin system never
injects Go code.** Static `CGO_ENABLED=0` binaries rule out Go `plugin`, so
extensions are manifests wired into seams that already exist. That limitation
turned out to be a feature — it forces the seams to be real.

---

## 3. Cloud-native by construction

The same code runs single-process on a laptop and multi-replica in a cluster.
That property survives only if **every feature is designed for N replicas from
its first commit** — high availability and horizontal scale are architecture,
not a migration you schedule later. Reference:
[cloud architecture](cloud-architecture.md) ·
[cloud deployment](cloud-deployment.md).

### Every replica is disposable; ownership is elected explicitly

No in-process global is ever the authority for concurrent work. When two
replicas could both act, something must elect one — and the codebase already
ships the vocabulary for it. Reuse these rather than inventing a fifth:

| Primitive | Where it runs | What it guarantees |
|---|---|---|
| **NATS-KV lease** per run id (`iterion-run-locks`, TTL 60s, refreshed every 20s) | runner pods ([loop.go](../pkg/runner/loop.go)) | one owner per run — and a **single failed refresh makes the runner self-cancel** rather than risk split-brain |
| **NATS queue group** | `usernotify` dispatcher | exactly one replica handles each event |
| **Per-tenant CAS cursor** | the cloud `board_events` tail ([trigger_cloud.go](../pkg/server/trigger_cloud.go)) | one publishing replica per tenant |
| **Mongo CAS** | `cloudsched` ticker, [`orgusage`](../pkg/orgusage/) counters, the `sent_notifications` first-writer-wins claim, `boardmongo.ConsumeLabels` | each due schedule fires exactly once; counters don't lose writes; a label-consuming trigger can't double-launch |
| **Valkey / Redis** (single-node or Sentinel-HA) | [`pkg/valkey`](../pkg/valkey/) | *ephemeral* cross-replica state: forge OAuth/CSRF, board-MCP run tokens, auth rate-limit buckets |

### Horizontal scale is "more pods"

Per-pod serialization, never a fleet-wide cap. The runner pool scales out
under KEDA and each pod holds one in-flight run through its serial loop;
`MaxAckPending` is a fleet ceiling that must stay **above** the max pod count.
The historic value of `1` — which pinned the entire fleet to a single
concurrent run — is the anti-pattern worth remembering, because it looked like
a safety setting.

### Restart is normal, not exceptional

- A pod dying mid-run resumes from its **checkpoint**; the run does not start
  over ([resume](resume.md)).
- Subbots **reattach** across a restart
  ([ADR-084](adr/084-subbot-reattach-across-restarts.md)).
- The **orphan sweeper** catches what the runner cannot — a pod that died
  before claiming or before its first status write — and CAS-flips those runs
  to `failed_resumable`.
- A rolling deploy **drains** rather than kills: `config.runner.drainMode`,
  a PodDisruptionBudget, and a termination grace period sized for the drain
  ([cloud deployment](cloud-deployment.md)). Restarting a runner mid-run to
  ship a fix is a measured way to lose hours of work.

### A lossy bus needs a reconciliation net — and both paths must be idempotent

The event spine is deliberately lossy where losing an event is survivable, so
every consumer pairs a fast path with a net:

- `usernotify` replays dropped episodes with a 2-minute reconciliation sweep,
  deduped by the per-episode claim.
- The board fast path stamps a card so the dispatcher's `Claim` picks it up
  now; the 30s poll stays the net. **Because the claim is atomic, fast path +
  poll cannot double-launch** — that is the shape to copy, not "pick one path".

### No feature ships local-only

A filesystem-only durable seam is a **cloud hole**, not a "known limitation".
[ADR-073](adr/073-cloud-twins-for-fs-only-run-detail-seams.md) — after
[ADR-067](adr/067-persist-run-git-metadata-for-cloud-panels.md) and
[ADR-068](adr/068-persist-run-diff-content-for-cloud-panels.md) — had to close
three of them retroactively, each one a feature that silently degraded or
503'd for cloud users while looking shipped.

So: **new durable state gets its cloud twin in the same change**, behind the
same interface — the fs/Mongo store pairs, `InProcBus`/`NATSBus`, plus a
memory implementation for tests. This is pillar 2 paying off: because the seam
exists, the twin is an implementation, not a rewrite.

Local-only remains a *deliberate* choice for host-bound ergonomics — the
desktop app, the post-mortem PTY shell, the host crontab integration. It is
never the default for durable state or a control-plane surface.

### Before merging anything stateful, answer four questions

1. Who owns this when there are **three replicas**?
2. What happens if the pod dies **mid-work**?
3. Does it survive a **rolling deploy** — drain, or lose work?
4. Is the **cloud twin in this PR**?

---

## 4. Git-native is first class *(pre-arbitrated)*

Iterion is a **code forge / factory**. Its git-shaped capabilities are not
legacy to be abstracted away beneath a product layer; they are the product for
a large part of the audience. First class, by name:

- The `.bot` workflow as **readable, diffable, PR-reviewable text** — the
  reason a recipe becomes a versioned artifact instead of tribal knowledge.
- **`worktree: auto`** and its finalization: a persistent
  `iterion/run/<name>` branch as the GC guard, then a best-effort fast-forward
  of the operator's checked-out branch (`--merge-into`, `--branch-name`).
- **Review scope** anchored at `refs/iterion/runs/<run>/gate/<seq>` — a human
  gate shows everything changed since the previous gate, as a git range
  ([review-scope](review-scope.md)).
- **Forge integrations** (GitHub / GitLab / Forgejo), PR review, the
  [merge gate](merge-gate.md), inbound [webhooks](webhooks.md),
  [repo-scoped provisioning](forge-integrations.md).
- The **right-artifact discipline** — judge the working tree (`git diff HEAD`,
  or `git diff <base>`), make untracked files visible before diffing — and the
  **commit-in-stride** campaign contract, where git *is* the run's state.

### The rule

**No product-oriented surface may degrade or bypass these.** A view
*projects* the git truth; it never becomes a second source of truth for it.

### Where git cannot serve, add a parallel mechanism — don't remove git

Two shipped precedents, both worth copying:

- [**Workspace versioning**](workspace-versioning.md) — the default run has no
  worktree: the workspace *is* the operator's checkout, so `git add -A` would
  stage their own uncommitted work. `pkg/workspacetrack` therefore captures
  content-addressed snapshots **alongside** git, and `worktree: auto` runs keep
  using per-node git snapshots. Two mechanisms, each where it belongs.
- **Greenfield `app-dev`** — an empty, non-git directory is a legitimate
  starting point: `worktree: auto` degrades to in-place, and the bot `git
  init`s and commits from slice 0. Git absent at t₀ is not git abandoned; it
  is git *created*.

---

## 5. Product-oriented views are welcome — additively *(pre-arbitrated)*

Agent work is operated by more than developers, and it is not only about code.
Both directions are legitimate and neither needs to argue for its right to
exist:

**(a) Surfaces for non-dev roles**

- The **pipelines control center** — one global board of running pipelines and
  the human interactions they are blocked on
  ([ADR-071](adr/071-board-as-pipeline-projection.md),
  [ADR-074](adr/074-dedicated-pipeline-board-projection.md)).
- The **native kanban board**, the [session board](session-board.md)'s
  declarative semantic widgets, [notifications](notifications.md), and the
  [repo-first studio shell](repo-scope.md).

**(b) Non-code use cases as first-class bots**

- **feed-watch / Vigie** — feed collection, semantic dedup, editorial digest
  to chat. No repository involved at all.
- **wiki-gen**, **rgaa-audit** — documentation and accessibility.
- **bmady** — Analyst → PM → Architect → Dev → QA with a human gate between
  every phase: product management, with code as the last step rather than the
  whole story.

### The arbitration

ADR-071 already wrote it, and it generalizes: **an additive projection, not a
replacement.**

A product view is a **read model** over execution and over git. Making it a
mutable authority duplicates state — which is exactly the argument ADR-074
used to refuse a `board.json` per bot: it would have re-created the dispatcher's
concurrency and migration problems for a view.

So, concretely:

- ✅ Don't reject a feature because "iterion is a dev tool". The audience is
  anyone who operates agent work.
- ✅ A view may derive its columns, cards and states from runs, checkpoints and
  git — and may offer actions that route back through the existing authority
  (the dispatcher's `Claim`, `runview.Service.Launch`, the interaction store).
- ❌ Don't make git optional *in the core* to make a view simpler.
- ❌ Don't ship a view that needs the engine to know a specific bot — thread it
  through the generic seams (pillar 2).

---

## How to use this page

Five questions, at the five moments they matter:

1. **About to add a limit?** Artificial or load-bearing? If load-bearing, what
   is its escape hatch, and is that hatch documented and greppable?
2. **About to add a `switch` arm, an `if botID ==`, or an enum value?** Which
   seam is missing, and is this the second variant — i.e. the moment to build
   it?
3. **About to add durable state or a background loop?** Who owns it across
   three replicas, what happens if the pod dies mid-work, does it survive a
   rolling deploy, and is the cloud twin in this PR?
4. **About to touch a git path?** Is the git-native behaviour preserved for the
   people who rely on it, with any new mechanism running *alongside* it?
5. **About to add a view?** Is it a projection over an existing authority, or
   is it becoming a second source of truth?

Related reading: [Why Iterion?](why-iterion.md) ·
[Architecture](architecture.md) ·
[The ratchet](improvement-ratchet.md) ·
[Workflow authoring pitfalls](workflow_authoring_pitfalls.md) ·
[ADR index](adr/)
