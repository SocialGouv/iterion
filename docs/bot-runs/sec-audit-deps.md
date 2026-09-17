# Depsy — `sec-audit-deps` run bilans

Universal supply-chain / SCA auditor: enumerates installed deps per ecosystem,
runs heuristics + CVE baseline, LLM-reviews, emits findings to the board.
Read-only (no code edits). See [bots/sec-audit-deps/](../../bots/sec-audit-deps/).

> **Known status (CLAUDE.md):** the CVE floor is now **real** —
> `run_generic_heuristics` runs `trivy fs --scanners vuln` over the workspace from
> a bare checkout, matching every pinned version in go.mod / package-lock.json /
> requirements.txt / Cargo.lock etc. against the OSV/GHSA/NVD DB, and feeds those
> findings into `heuristic_join` for the reviewer. Still pending (native:3a81df64):
> the per-ecosystem install-tree heuristics (npm-audit/pip-audit) and code-pattern
> /typosquat-corpus malware signals — so a run still self-labels with a
> "⚠ Coverage" banner for partial coverage, but it is no longer a 0-finding scaffold.

## 2026-09-17 — profile 2: enumerate and review prompts reach the models with their paragraphs (run 01a0aef1-77e6)

- Status: **validated** — the whole pipeline end to end on a dependency-free
  scratch repository, an honest zero.
- Versions: bot sec-audit-deps 0.1.2 (`dsl: 2`, wave 2 of #1344) · iterion
  `853b5ce28` for the bots; the engine the branch binary `v3.154.1+d5b7db09f` carrying the first wave-1 runtime fix (main was at v3.155.0) · the bot's own
  sec sandbox image (`iterion-sandbox-sec:edge`, present locally) ·
  `--backend claude_code --model claude-opus-5` on every LLM node (the GPT
  forfait the bot routes to by default is spent until 2026-09-20), served
  through the host's Anthropic-compatible facade (z.ai).
- Method: CLI `iterion run <bundle>/main.bot` launched FROM a scratch copy of
  the greet project (a one-file Python CLI, no lockfile), `--store-dir` the
  operator's workspace store, `--var severity_threshold=high`, caps
  `--max-cost-usd 5 --max-duration 25m`, `ITERION_BIN` the branch binary.
- Result: **finished**, ~10 min, **$0.74** (enumerate_deps $0.23 · llm_review
  $0.51). `enumerate_deps` found no manifest and said so; the ecosystem
  heuristics ran in 59 ms, the generic heuristics (trivy over the tree) in
  300 s; `llm_review` reviewed 0 packages — "pending[] was empty and both
  heuristics returned zero signals with zero errors — verified correct" — and
  wrote the report to the run's `.sec-audit/deps-findings.md`; no issue
  created, the cache updated.
- Value: the pipeline live on profile 2 — `enumerate_deps` → `normalize_deps`
  → `run_eco_heuristics` → `run_generic_heuristics` → `filter_cached` →
  `llm_review` → `update_cache` → `done`; the run's `events.jsonl` carries the
  rendered review prompt with `per-package verdicts.\n\nIMPORTANT — UNTRUSTED
  INPUT BOUNDARY`, the paragraph break the profile-1 lexer used to fold.
- Findings / misses: a repository without dependencies proves the graph and
  the prompts, not the reviewer's judgement on a real package; the coverage
  banner logic was not exercised (no ecosystem detected).
- Engine hardening: none needed.
- Lessons for next run: pick a scratch repository with one lockfile to see a
  package reviewed; the generic trivy pass costs five minutes even on three
  files.

## 2026-06-13 (retest) — 2 engine bugs root-caused & fixed (runs 019ec1b1→019ec1d3)

- Status: **engine-unblocked.** Sandboxed claw now runs end-to-end; the SCA
  pipeline reaches and completes `enumerate_deps`. Bot remains scaffold-limited.
- Versions: bot sec-audit-deps 0.1.0 · iterion 778b9860 / bbdca0da / ea61817a
- Method: `./iterion run bots/sec-audit-deps/main.bot --var severity_threshold=high
  --store-dir .iterion`, sandboxed (`iterion-sandbox-sec:edge`), run **alone**.
  Tested `anthropic/claude-sonnet-4-6` (`--var`/`ITERION_SEC_AUDIT_MODEL` override)
  and the default `openai/gpt-5.5`-via-forfait.

### The original "hang" was THREE separate things — none was a true hang
1. **019ec17e was confounded.** It errored `multiplexer: envelope line exceeds
   MaxEnvelopeLineBytes`, then was drained by a concurrent `task studio:dev`
   restart (`server drained`). The envelope error did **not** reproduce on clean
   CLI retests — most likely the draining studio corrupted the host↔runner IPC,
   not a standalone bug. Lesson: dogfood sandboxed bots via **CLI, alone**, not
   alongside a churning `studio:dev`.
2. **TLS-inspection proxy hangs every streaming LLM call (FIXED 778b9860).** The
   Layer-2 secret-egress proxy is on by default whenever a run carries secrets
   (LLM creds qualify — confirmed via `ITERION_SANDBOX_TLS_INSPECT=off` making the
   call complete). It MITMs egress and kept the inspected client connection alive
   after a close-delimited streaming response → the in-container claw client
   blocked forever on the LLM call. Reproduced 3× (gpt-5.5 forfait + anthropic).
   Fix: force `Connection: close` per inspected response (streams still flush).
3. **claw empty tool-result → anthropic 400 (FIXED bbdca0da + sibling 248882e).**
   An empty tool output (a `grep` with no matches) serialised to a tool_result
   whose nested text block dropped its `text` field via omitempty →
   `messages.N…tool_result.content.0.text.text: Field required`. Fix: force the
   `text` field present in `ContentBlock.MarshalJSON` (mirrors the existing
   tool_use.input fix). Validated: the agent ran 25 tool steps to completion.

### Validated
- With both fixes, `enumerate_deps` (anthropic) **completed**: 481s, **170,762
  tokens**, 25 tool steps. The sandboxed-claw delegate path now works end-to-end
  through the inspect proxy. (gpt-5.5-forfait default run 019ec1d3 launched to
  confirm the as-shipped config; the engine path itself is proven.)

### Remaining (bot/claw-level, not engine)
1. **enumerate_deps is slow + expensive** — 8 min / 170K tokens just to enumerate
   deps. The agent ingests far too much despite the skill's "don't read lockfiles"
   guidance. Tighten the prompt/skill or cap `tool_max_steps` (currently 25).
2. **structured-output fallback retry** — the final structured output
   (deps/ecosystems/summary) fell back to the text wrapper missing required fields
   → retry. A claw structured-output-with-tools robustness gap; affects any claw
   agent node with a schema + tools (Seki too).
3. **board emit needs the server** — sandboxed `board.*` caps require the HTTP
   board transport (C082); a bare CLI run can't post findings. Run via the
   studio/server.
4. **SCA scaffold unchanged** — heuristic layer still discards real scanner output
   (native:3a81df64); treat output as enumerate + LLM-review only.

### Default model (gpt-5.5-via-forfait) is unreliably slow here
- Run 019ec1d3 (default `openai/gpt-5.5` via ChatGPT forfait, fixed engine, inspect
  ON): `enumerate_deps` ran **>12 min with no completion** and was killed — well past
  the 8-min anthropic baseline. The engine path is proven (anthropic completed); the
  default forfait model is the bottleneck. **Recommend** overriding to
  `anthropic/claude-sonnet-4-6` (`--var enable… ` / `ITERION_SEC_AUDIT_MODEL`) for a
  reliable enumerate, or a faster default — and capping `tool_max_steps`.

### Lessons for next run
- Run via the **studio** (server up, board transport wired) + alone + fixed binary.
- Cap enumerate cost — 170K tokens for dep enumeration is excessive.
- Prefer `anthropic/claude-sonnet-4-6` over the gpt-5.5-forfait default until the
  forfait path's latency is understood.

## 2026-06-13 — iterion self-audit dogfood (run 019ec17e)

- Status: **inconclusive — hung at `enumerate_deps`, cancelled.** *(Superseded by
  the retest above: root cause was the TLS-inspect proxy hang + a studio-drain,
  not parallel load.)*
- Versions: bot sec-audit-deps 0.1.0 · iterion (post-fixes, static binary installed)
- Method: `POST /api/runs`, `severity_threshold=high`, sandboxed
  (`iterion-sandbox-sec`). Launched in **parallel** with a Featurly re-run.
  Read-only (no `remediate`/`patch_author`/`worktree:` — confirmed safe: it can't
  self-kill or pollute the tree the way Seki's remediation did).
- Result: the static-binary + `backendIsClaw` fixes meant the sandboxed claw
  runner started (no "iterion not found" — note Depsy uses literal `backend:
  "claw"`, so it was only ever blocked by the static-binary issue, not the
  env-template one). But `enumerate_deps` (the first node, claw/gpt-5.5)
  **hung**: ~28 min with a single in-flight LLM call, `last_seq` stuck at 6, no
  tool calls, no retries logged. Cancelled.

### Findings / misses
1. **`enumerate_deps` stalls (medium — reliability, needs a clean repro).** One
   gpt-5.5 enumeration call hung with no progress/retry. Plausible contributors:
   (a) resource contention — two sandbox-sec-class containers + multiple LLM
   calls were running in parallel (Seki had just finished, Featurly2 was live);
   (b) the enumeration prompt ingesting large manifests (the bot's own skill warns
   against reading lockfiles — worth checking it doesn't); (c) a transient claw /
   ChatGPT-forfait stall. Re-run **alone** (no parallel sandbox bots) to isolate.
2. Did not reach `llm_review` or board emit, so the SCA path + the known scaffold
   gap (#native:3a81df64) remain **unvalidated** on iterion this pass.

### Lessons for next run
- Run Depsy **alone**, not alongside another sandbox-sec bot — the parallel load
  is the most likely cause of the `enumerate_deps` stall.
- It is read-only and worktree-free → safe under `task studio:dev` (unlike Seki's
  remediation), so no special isolation needed; just give it dedicated resources.
- Still gated behind the documented scaffold caveat: treat output as
  enumerate + LLM-review, not a complete dependency audit, until native:3a81df64.
