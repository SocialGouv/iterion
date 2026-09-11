# Copi — `bots/copilot`

Conversational iterion assistant: the DSL, the Cxxx diagnostics, run/resume
semantics, backends. Read-only by construction. Newest run first.

---

## 2026-08-31 — host-event resumes survive the HTTP acknowledgement

- **Observed defect:** Studio could acknowledge an executed Copi action with
  `action-completed`, then immediately cancel Copi because the resumed engine
  inherited the completed HTTP request context. Its target watch stopped with
  it, breaking the `target → Copi → CLI` supervision chain.
- **Repair:** `handleDeliverHostEvent` now detaches the engine lifetime while
  preserving request-scoped values, matching the existing human-answer path.
  Copi also recognizes host receipts by `kind`, correlates a completed action,
  verifies it through read evidence and retains the confirmed result in its
  brief before requesting the next safe step.
- **Verification:** the server lifecycle regression closes the HTTP response
  before a deterministic resumed turn finishes. On the live `:4893` Studio,
  Copi `01a05819-7366-7ede-8142-e9c5623f22bf` requested watch
  `b2e82db0-3185-449f-b4b1-2e1fbb393873` for town_planner
  `01a04ee1-e661-71e6-a130-a222f994862d`; its failure event woke Copi, then
  the exact production receipt `{action, args, message}` was delivered. Copi
  correlated `action: run.watch`, re-read the target, retained the confirmed
  watch in its brief and returned to its chat pause without cancellation.
- **Prompt migration:** the prior Copi requested its watch be stopped before
  the on-disk prompt hash changed; it remains parked as evidence, while the
  fresh Copi owns the active watch. This avoids a stale assistant silently
  failing its next resume on the workflow hash guard.

---

## 2026-08-30 — complete diagnostic evidence without an unguarded shell

- **Observed defect:** Copi could inspect bounded Iterion run events but could
  not search the live workspace by content, routinely missed project-owned
  evidence under `state/`, and received no warning when a large source read
  crossed a downstream 256 KiB clipping boundary. DB-backed project state was
  unreachable. Separately, the ambient `POSTGRES_PASSWORD=shorts` registered
  `shorts` as a global literal secret and erased thousands of unrelated source
  occurrences from persisted observability.
- **Workspace evidence:** claw `read_file` now returns at most 240 KiB with an
  explicit line continuation marker and accepts `start_line`/`line_count`.
  `workspace_grep` is confined to the active workspace, searches ignored
  project artifacts such as `state/`, and excludes credential files and
  internal run stores before scanning.
- **Live evidence:** native Bash and Grep remain denied by default. Copi alone
  declares a `diagnostic_shell` alias under an explicit `ask` rule. On Claude
  Code its declared, single-line Bash verification request is mapped to that
  alias only after explicit-deny screening, so the operator approves the full
  exact command. The bridge permits bounded diagnostics and source-
  nonmutating targeted tests/validation, never Git/source writes; a multiline
  command or any other node remains native Bash → deny.
- **Redaction:** short/simple ambient values are no longer eligible for global
  substring taint. Credential-file denies, launch-env name redaction and the
  permission boundary still keep those values out of ordinary model inputs;
  distinctive generated passwords/tokens continue through known-value
  redaction.
- **Verification:** tool tests pin explicit large-file continuation,
  credential-safe search and outside-workspace refusal; permission tests pin
  native Bash/Grep deny versus diagnostic-shell ask; secretguard tests cover
  both the `shorts` collision and a distinctive ambient secret. The Copi bot
  validates with only its intentional C128 `sandbox:none` warning.

---

## 2026-08-29 — tool-pair-safe persistent conversation (run `copi-toolpair-fix-smoke-20260829`)

- **Status:** validated on the Tabarria Studio at `:4893`; the smoke run was
  cancelled deliberately after its third successful chat pause.
- **Versions:** Copi from `feat/assistant-epic` · Iterion
  `beb4568ac569-dirty`, rebuilt statically with the tool-pair repair.
- **Method:** three real operator turns with `reviewer=on`, OpenAI claw for
  `copi`/`revise`, Claude Code for `review`, `sandbox:none`, no supervisors and
  no host actions. Turns two and three explicitly referred to the preceding
  answer to exercise the shared `assistant_conversation` slot.
- **Result:** all three passes completed `copi → review → revise → compose →
  chat`; the same claw session id (`ee4063e8-…`) survived every turn, revise's
  request grew from 6 to 12 to 18 messages, and no OpenAI function-output 400
  or fallback occurred. The final persisted envelope contains 20 messages and
  seven exactly matched `tool_use`/`tool_result` ids.
- **Engine hardening:** compaction now prunes orphaned tool blocks in both
  directions without widening the retained window; loading repairs historical
  envelopes; request construction validates the invariant; `ask_user` keeps
  only its explicitly pending id. Fallback restores the pre-attempt snapshot
  under the effective named slot instead of wiping the conversation. The exact
  4893 summary/result boundary and tight-budget shrink are covered
  deterministically in `pkg/backend/model/session_toolpairs_test.go`.

---

## 2026-08-29 — durable conversational continuity

- **Observed defect:** Copi used `session: inherit_if_available` on `claw`, but
  claw returned no `_session_id`; every ordinary chat resume therefore ran
  fresh. The edge wiring was dead and `context_brief` was the only memory.
- **Host projection:** the manifest's chat node now requests a canonical
  `conversation_history` projection. The server rebuilds it from the existing
  run events, keeps the newest eight messages under an estimated 12k-token
  cap, and injects it as an ephemeral host input. It is not copied into
  `human_answers_recorded` or an artifact, so events remain the authority.
- **Verbatim continuity:** claw now packs its compacted `api.Message` history
  into the existing backend-session store for `session: persist`. Copi and its
  private revise pass share `session_slot: assistant_conversation`, ensuring
  the next turn continues from the answer shown to the operator rather than
  the pre-review draft.
- **Fail-safe:** a provider-fingerprint change discards the opaque session and
  falls back to the bounded projection. A present-but-empty continuity key is
  now a visible `session_degraded` event instead of an Info-only fresh start.
- **Verification:** deterministic tests cover host-input non-persistence,
  named-slot hand-off, claw envelope round-trip/size cap, history bounds and
  missing-session observability; `iterion validate bots/copilot/main.bot`
  passes (only the pre-existing C128 sandbox opt-out warning).

---

## 2026-08-29 — cross-project failed-bot delegation

- **Status**: deployed on 4893/4894; deterministic validation and a zero-LLM
  live delegation smoke pass.
- **Scope**: Copi `0.1.7`, Featurly `2.4.0`, generic run provenance and launch
  contracts. No bot id is introduced in `pkg/`.
- **Contract**: Copi stays read-only and emits `run.launch` with a catalog
  worker plus `source_run_id`. The host resolves the failed bot's owner repo,
  injects a bounded `run_failure` envelope and operator instructions, pins a
  snapshot commit, requires the worker's declared `worktree:auto`, and forces
  `merge_into:none`.
- **Isolation**: dirty/untracked source content is captured through a temporary
  Git index without changing the operator checkout/index. A deterministic
  per-attempt worker run id makes the run store's unique create the CAS for one
  active repair per failure fingerprint. Consumer re-pin and merge remain
  separate confirmed operations.
- **Return path**: the Studio watches worker `finished|failed|cancelled`
  outcomes and the existing safe host-event wake returns the result to Copi.
- **Verification**: Go tests cover provenance persistence, install sidecars,
  dirty-tree snapshot isolation and outcome-kind selection; Vitest covers the
  closed action payload and host-selected terminal watch. Both modified bots
  pass `iterion validate`. Live run
  `repair-b5b67a91d83531344432d38b-1` resolved legacy run
  `01a0451b-dead-700f-8fcb-99b60c155c78` to `iterion-bots`, snapshotted all
  38 dirty/untracked entries at hidden commit `092ceea8a0d6`, ran in an
  isolated worktree and finished without changing the source checkout or its
  HEAD (`e972a8a308d2`).

## 2026-08-29 — durable target-run watch and safe automatic diagnostic wake

- **Status**: implementation and deterministic validation complete; live
  failure smoke is pending deployment below.
- **Scope**: Copi `0.1.6`, generic chat manifest protocol, local/Mongo control
  plane, Studio action/UX. Existing native-ticket watches are unchanged.
- **Contract**: `run.watch` creates a rooted run-tree → assistant-run link in
  `diagnose` or `propose` mode. Descendant actionable outcomes are included;
  successful child completion stays silent and root Done alone resolves it.
  Outcome events are the fast path and a durable tree sweep is the backstop.
  Episodes use a CAS lease, concrete outcome-run provenance, failure fingerprint,
  cooldown, maximum count and conversation-budget guard.
- **Wake boundary**: delivery is accepted only while the assistant is
  `paused_waiting_human` on its manifest chat node. Running turns stay pending;
  mid-turn `ask_user` and `paused_operator` are not resumed. Cancelling or
  finishing the assistant stops its watches; minimizing the dock does not.
- **Authority**: the resume carries a JSON `host_event` field, not the chat
  `message`. The transcript renders it as an automatic host event and Copi's
  prompt forbids treating it as explicit intent. `auto_safe` is rejected: a
  browser/localStorage action policy is not server authority.
- **Verification**: FS store tests cover idempotent episodes, one winner under
  concurrent claims and lease recovery. Server tests pin chat-vs-ask_user/
  paused_operator eligibility, the 90% budget guard and stable fingerprints.
  Studio tests pin host-selected assistant ids, rejection of `auto_safe`, and
  the absence of an operator message for watch wake-ups. Go package tests,
  Copi validation, TypeScript and targeted Vitest pass.

## 2026-08-29 — closing an assistant conversation stops its owned run

- **Status**: deterministic validation complete; production Studio rebuilt and
  reloaded on ports 4893 and 4894. Browser close smoke pending an operator
  gesture so no existing run is cancelled without confirmation.
- **Scope**: Studio assistant lifecycle; Copi bundle remains `0.1.5` on branch
  `feat/assistant-epic`.
- **Trigger**: closed conversation tabs could leave Copi runs in
  `paused_waiting_human`. The close path read `runId` and status only from an
  in-memory snapshot, fire-and-forgot `cancelRun`, swallowed every error, then
  deleted the tab and store immediately. A not-yet-hydrated or stale tab sent
  no cancellation at all. Bot switching dropped the persisted id without any
  cancellation, and a single conversation exposed only Minimise, no stop
  control.
- **Implementation**: close, new-session and bot-switch now share one
  cancel-before-dispose contract. It prefers the conversation-owned run id,
  never gates on client status, treats 404/410 as already gone, waits for HTTP
  acceptance, and retains the owner with a persistent Retry toast on real
  failures. A synchronous memory-only guard deduplicates gestures; destructive
  live/resumable disposal asks for confirmation. The single-conversation strip
  now exposes a visible **Close conversation and stop its run** cross. The
  dock's minus remains pure minimisation.
- **Verification**: pure tests cover id resolution, confirmation classification,
  delayed acceptance, 404/410 and 5xx. Provider tests exercise absent snapshots,
  deduplication, retained tabs on error and bot switching. Component tests pin
  the single-tab affordance and minimisation semantics. The complete Studio
  suite passes (192 files, 1,700 tests), as do TypeScript and the production
  build; both live servers load the rebuilt asset containing the new close
  contract.

## 2026-08-29 — assistant resume handles workflow source drift

- **Status**: validated locally and deployed to the Studio instances on ports 4893 and 4894.
- **Versions**: bot 0.1.4 → 0.1.5 · branch `feat/assistant-epic`.
- **Trigger**: Copi emitted an explicit `run.resume` for cancelled run
  `01a044da-f33b-712c-b167-0b6ed6795c66`. The server correctly refused because
  the current workflow hash differed from the launch hash, but the assistant
  card reduced that guard to `API error 400` plus a `Retry` button that could
  only repeat the same unforced request.
- **Implementation**: assistant resume now mirrors the Pipelines board's
  two-step source-drift path. It first sends `{}`; the exact source-change
  verdict transitions the card to a warning with **Resume with updated
  workflow**; that second explicit gesture sends `{force:true}`. Other resume
  errors remain ordinary errors.
- **Authority boundary**: `force` is not accepted in the Copi action contract.
  The validator strips a model-supplied value, and only host context from the
  second operator gesture can add it to the API request. Auto-action policy may
  attempt the normal resume, but can never auto-force through source drift.
- **Verification**: request-boundary tests prove model `force:true` is stripped
  and host force is honored; action-card tests exercise the 400 → warning →
  second gesture → forced resume sequence; the existing Pipelines tests keep
  the same behavior pinned. Typecheck passes.

## 2026-08-29 — cross-review closes the loop before publication

- **Status**: validated — deterministic suites plus one live non-empty-review
  turn after the static rebuild/reload of both studios.
- **Versions**: bot 0.1.3 → 0.1.4 · branch `feat/assistant-epic`.
- **Trigger**: with `reviewer: on`, the chat displayed Copi's draft followed by
  a separate « Revue croisée » block. Copi never received that feedback: the
  graph was `copi → review → compose`, and `compose` concatenated the two
  strings deterministically.
- **Implementation**: the reviewed path is now
  `copi → review → revise → compose → chat`. `revise` uses Copi's model,
  fallback ladder and tools; it inherits Copi's backend session when one is
  exposed, with the explicit question/draft/brief as the cold-session fallback.
  It challenges the private critique and returns the only operator-visible
  answer. `compose` is now a plain delivery projection, never a text
  concatenator, and carries the revised `context_brief` into the next turn.
- **Integrity**: the revision pass may change conversational prose, quick
  replies and rolling memory only. Typed host actions, editor proposals and
  companion-file replacements remain the first Copi pass's published requests,
  so an editorial model cannot replace a host-policy-checked action after the
  fact. The prompt requires the prose to stay aligned with those immutable
  requests and preserves deterministic validation verdicts verbatim.
- **Verification**: the graph contract checks the same-family author session,
  private critique edge and absence of critique concatenation. The runtime E2E
  supplies a deliberately wrong draft plus a non-empty critique and proves that
  the chat receives only Copi's corrected answer; after resume it proves the
  next turn gets the revised brief and session rather than the draft state.
- **Live result**: run `copi-review-refine-20260829` on the `:4894` project
  produced a substantive reviewer critique, entered `revise`, rechecked the
  review with `runs.read`, and parked on `chat` with only `outputs.revise.reply`.
  Neither the original draft nor the critique/« Revue croisée » label appeared
  in the delivered question payload. This CLI replay intentionally tested the
  editorial topology, not card resolution (host-attested page context is added
  by the Studio send path, not by a raw CLI `--var initial_message`). Claw
  exposed no resumable session id on this turn, exercising the explicit
  question/draft/brief fallback successfully.

## 2026-08-29 — run diagnosis uses host-attested context (`:4893`, `:4894`)

- **Status**: validated — deterministic suites plus one live Copi turn after a
  static rebuild and reload of both studios.
- **Versions**: bot 0.1.2 → 0.1.3 · branch `feat/assistant-epic`.
- **Trigger**: Copi answered that it could not inspect a failed run in two
  studios. On `:4893`, run `01a04943-36a8-7629-ba07-9d309787fab7` existed in
  the server-selected global project store but Copi globbed only the workspace.
  On `:4894`, `native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf` was a task
  reference; Copi incorrectly treated its UUID as a run id instead of following
  `last_run_id` to cancelled run `01a044da-f33b-712c-b167-0b6ed6795c66`.
- **Root cause**: the assistant had typed pointers but no host-resolved facts;
  its run-debug skill suggested a glob that cannot cross the workspace
  boundary, while the claw node exposed only workspace reads. The optional
  reviewer could identify the mistake only when it happened to reach the
  external store, and could not repair the answer already returned.
- **Implementation**:
  - the Studio resolves attached/active `run`, `node`, and `card` references,
    plus explicit `native:` task mentions, at send time and stamps a bounded
    `<resolved-assistant-context>` containing task state, `last_run_id`, run
    status, failing node, error code and error;
  - a bot-agnostic `runs.read` capability exposes read-only `run_get`,
    `run_events`, and `runs_list` over the current `RunStore`; claw calls stay
    in-process, non-sandboxed delegate calls use stdio, and sandbox/cloud calls
    use the existing ephemeral host-MCP listener with the run tenant pinned in
    the token grant;
  - Copi and its reviewer use those tools and are explicitly forbidden from
    reconstructing or globbing `.iterion` store paths;
  - a selected Pipelines drawer contributes its exact card/run typed reference,
    so navigate-then-send quick replies resolve after the drawer is active.
- **Isolation**: no store directory, workdir, run inputs or arbitrary task body
  is added to the prompt. Local project isolation remains host-owned; cloud
  reads are tenant-scoped even though MCP token requests do not carry an
  operator JWT.
- **Verification**: Go tests cover the resolver, capability gate, bounded run
  projection, in-process tools, HTTP tenant pinning and backend/runtime wiring.
  Studio typecheck plus context/page/dock tests pass. A Copi botreplay golden
  requires the stamped failed status and failing node without a live model key.
- **Live result**: studios `:4893` and `:4894` were reloaded on Iterion
  `beb4568a`; both resolver calls returned the expected run/task facts. Live
  run `01a04cb1-e8b3-71b1-90d4-d699dabe8965` on `:4893` called `run_get` and
  `run_events`, cited `run_failed` seq. 141, identified internal node
  `seal_experience` and missing `experience-map.a2.candidate.json`, and warned
  against a blind Resume. It did not glob or reconstruct `.iterion`.
- **Live-run hardening**: that replay exposed an oversized raw event page
  (~770 KB). Before finalizing, `run_events` was changed to a 192-KB bounded
  diagnostic projection (max 250 events/page) that omits inputs, prompts and
  arbitrary tool/node outputs; a regression test seeds all three and proves
  they do not cross the capability boundary.
- **Follow-up regression**: a task id typed directly in prose
  (`#native:d5fc…`) from the generic Pipelines page bypassed the first
  implementation, which only resolved active or attached chips. The dock now
  asks the host resolver on every free-text send; the host recognizes bounded
  `native:` mentions, resolves them as cards and follows `last_run_id`. A dock
  test pins this generic-page launch shape. Model-visible run-tool errors are
  also sanitized at the runops boundary, so a missing id reports only
  `run not found` and never the filesystem store path.
- **Follow-up live result**: after rebuilding and reloading both studios, run
  `01a04cc2-e099-784d-8f05-01b2407497ec` on `:4894` replayed the exact
  generic-page question. Copi followed the card to
  `01a044da-f33b-712c-b167-0b6ed6795c66`, called `run_get` and two pages of
  `run_events`, and correctly distinguished cancellation from failure: two
  `context canceled` delegate errors around `plan_macro` led to
  `run_cancelled` events 240 and 246.


## 2026-08-28 — cross-review without memory, reviewer probing the Studio API (runs `01a04999`, `01a049e1`, `01a049e4`)

- **Status**: validated — two fixes to the reviewer's input and prompt, measured on two follow-up runs.
- **Versions**: bot 0.1.1 → 0.1.2 · iterion `fbd56fc6` (worktree on `feat/assistant-authoring-files`; the fix lands on `fix/copi-reviewer-context` → `feat/assistant-epic`).
- **Method**: studio shorts (`:4894`, Copi loaded from the worktree via
  `--bots-path`), `reviewer: on`, Copi on `claw` + `openai/gpt-5.6-sol`,
  reviewer on `claude_code` + `claude-fable-5` (Claude Code 2.1.251). Two-turn
  scenario in the leading-question shape: « Je vais relancer la tâche pipeline
  native:… avec un reset, c'est bien ça ? » then « ok go ». Launched with the
  dock's own request shapes (`POST /api/runs` with `bot_id` + `vars`, then
  `POST /api/runs/{id}/resume` with `{answers: {message}, force: true}`), so
  the runs sit in the studio's store.
- **Result**:
  - before (`01a04999`, operator-reported): reviewer $0.93/turn; one denied
    `Bash curl http://127.0.0.1:4894/api/v1/…` per turn; critique opening with
    "I can't reach the Studio API from here"; at turn 2 « "ok go" ne porte
    aucun contexte » — it had been handed only the last message.
  - after v1 (`01a049e1`: brief on the edge + boundary stated in the prompt):
    $0.53 / $0.22; the turn-2 critique is contextual (« l'opérateur a déjà
    dit ok go … le brief lui-même dit que la confirmation a eu lieu »);
    still one denied call per turn — `ToolSearch select:Glob,Grep`, then a
    `Bash ls` on turn 2.
  - after v2 (`01a049e4`: `ToolSearch` allow-listed, `assistant_actions`
    passed): $0.80 / $0.15; turn 2 is an EMPTY critique ("Nothing to
    contest") — the reviewer saw the explicit `pipeline.task.reset` request
    matching the confirmed ask. No Bash attempt on either turn.
- **Value**: the reviewer now judges the answer as one message of the thread
  and can tell a typed proposal from an omission. Silence at turn 2 is the
  whole point: a reviewer that manufactures critique on a confirmed action
  trains the operator to skip it.
- **Findings / misses**:
  - `session: fresh` + an edge carrying only `operator_message`/`reply` is a
    reviewer with no memory. Copi's own `context_brief`, rewritten the same
    turn, held exactly the missing context — it now rides the edge. Assumed
    structural limit: the brief is authored by the model under review, so a
    shared misunderstanding is ratified, not caught. A raw bounded transcript
    is the follow-up if that ever bites.
  - Under `claude_code` the node's `tools:` list is inert; the deny gate is
    the only boundary, and a prompt that does not state it gets probed every
    turn. Stated, Fable 5 stopped curling the API.
  - Claude Code 2.1.251 ships NO Glob/Grep (`ToolSearch` answers "No matching
    deferred tools found"), so the workflow's `Glob` allow is inert on that
    backend — the reviewer can only `Read` exact paths. The prompt now gives
    the store layout (`<workspace>/.iterion/` or
    `~/.iterion/projects/<encoded-workdir>/`, cards at
    `dispatcher/issues/native__<uuid>.json`) so a Read is a lookup, not a
    guess — the v2 reviewer lost four Reads guessing `issues/<uuid>.json`.
    `ToolSearch` is allow-listed because a denied loader makes the model
    conclude the tool is gone and reach for Bash.
  - Copi (`gpt-5.6-sol`) emitted `run.reset` on `01a04999` turn 1 — not in
    the catalogue; the Studio would have rejected it. One line in the
    Host-actions catalogue ("there is NO `run.reset`"); both follow-up runs
    used `pipeline.task.reset`.
  - Scrubber side-effect worth knowing: the shorts project's
    `POSTGRES_PASSWORD` equals its directory name, so every path the reviewer
    touched reads `…/video/__ITERION_SECRET_env_POSTGRES_PASSWORD__/…` in the
    events. The masking works; the password should be rotated.
- **Engine hardening**: none needed — everything sat in the bot. Candidate:
  expose the resolved store dir to prompts (a `{{vars.store_dir}}` or an
  engine-provided ref) so a chat bot stops inferring it from the cwd.
- **Lessons for next run**: the leading-question test (22/08 below) needs a
  SECOND turn of the « ok go » shape — that is what exposes a reviewer without
  memory, and an empty turn-2 critique is the pass condition, not a failure
  to engage. Reviewer cost is dominated by its reads ($0.80 when it reads
  skills and probes paths, $0.15 when it has what it needs), so the
  store-layout hint is a cost lever as much as a correctness one.

## 2026-08-28 — declared companion-file proposal (run `01a04962`)

- **Status**: validated — paused normally after one Copi turn; no file was saved.
- **Versions**: bot 0.1.0 · iterion `8d8704bb` (branch `feat/assistant-authoring-files`, stacked on `feat/assistant-epic`).
- **Method**: local `legendary-film-chapter` editor marker with a complete live
  `main.bot`, a server-minted snapshot of 15 declared companion files, and an
  explicit request for one exact comment replacement in
  `film_pipeline/matter.py`. Reviewer off; Copi on `claw` +
  `openai/gpt-5.6-sol`.
- **Result**: Copi made one `read_file` call and emitted one bounded
  `file_changes` replacement with the exact host session/revision and
  `file_changes_intent: explicit`. The 4894 preview endpoint resolved it to a
  real before/after diff. A final disk read proved the original comment was
  still present; commit was deliberately not called. 522 unpriced tokens were
  reported for the answering node.

### Value and finding

The complete path works model → typed artifact → host-bound hash → server
preview without giving Copi a write tool or asking it to copy a SHA-256. The
server snapshot exposed exactly the manifest perimeter, including the three
workspace files needed by this project.

The active `main.bot` was 101,157 characters and therefore travelled inline
on this turn. That is correct for a live unsaved buffer and still under the
160-KB safety ceiling, but it dominates prompt size even when only a companion
script is edited. A future optimization can make an unchanged saved editor
document referenceable; V1 must keep inlining the unsaved buffer because the
run cannot otherwise read it honestly.

## 2026-08-22 — first dogfood, with cross-review on (runs `01a02a31`, `01a02a32`, `01a02a39`)

- **Status**: validated — after three attempts, two of which failed on real defects the bot's tests could not have caught.
- **Versions**: bot 0.1.0 · iterion `1ae6b850` (branch `feat/assistant-epic`)
- **Method**: `iterion run bots/copilot/main.bot --store-dir "$PWD/.iterion" --var reviewer=on --var initial_message=…`. Copi on `claw` + `openai/gpt-5.6-sol`; reviewer on `claude_code` + `claude-fable-5`. No board writes, no worktree (`worktree: none`), `sandbox: none`.
- **Result**: converged to the chat pause on every successful turn. 3 answering turns + 3 review turns. **$2.46 – $2.57 per reviewed turn**, essentially all of it the reviewer.

### Value — the reviewer earns its place, but not on every turn

Turn 1 ("explique C176") returned an **empty critique**. That is the designed
verdict for a sound answer, and it was correct: Copi's answer was accurate,
with `file:line` citations that matched what a human reading the same code
found independently.

Turn 2 was a leading question — *"je vais mettre `sandbox: none` et
`permission: off` sur tous mes bots de chat, c'est bien ça ?"*. The reviewer
bit, and usefully: it caught Copi **inverting the semantics of
`permission: deny`**, citing `docs/permissions.md:38`. Third run, same
question shape: the reviewer confirmed the answer and then added the caveat
Copi had left out — that a `permission:` boundary is only real on
claude_code, claw and pi, quoting `docs/permissions.md:157-159`.

So: silence when there is nothing to say, a specific and sourced objection
when there is. That is the behaviour the prompt asks for, and it held.

**The economics are the open question.** $2.46 to return an empty critique is
a real number, and on a standing conversation it compounds. Off-by-default is
right; whether an operator would ever leave it on for a long session is not
established by three turns.

### Findings — three defects, all found by running, none catchable by the tests

1. **`tools: [list_files]` — unknown tool, run dies on the first node.**
   `list_files` is not a registered claw tool; the registry rejects it at
   execution. `iterion validate` compiled it happily: **it does not check tool
   names against the registry**. The canonical builtins are
   `read_file, write_file, glob, grep, file_edit, web_fetch, bash`
   (`pkg/backend/tool/claw_builtins.go:91-98`), plus `skill` with a `skills:`
   block. A `Cxxx` diagnostic for unknown tool names on claw nodes would have
   turned a failed run into a compile error — worth a ticket.

2. **`concat()` is the ARRAY primitive; string joining is `+`.**
   `compose` and `gate` both used `concat(a, b, c)` on strings. It compiled,
   and **turn 1 passed** — `if()` short-circuits, and that turn's critique was
   empty, so the faulty branch never evaluated. The failure surfaced only on
   the turn where the reviewer had something to say, i.e. the first turn where
   the feature did its job. Pinned by
   `TestCopilot_CrossReview_ComposesBothHalves`, which drives the branch with a
   non-empty critique.

3. **`{{input.x}}` on an EDGE resolves against the RUN's inputs, not the source
   node's input — while the compile-time check validates against the source
   node's schema.** C034 rejects `{{input.reviewer}}` on `chat -> copi` because
   `chat_input` has no such field; at run time the same reference on
   `copi -> gate` reads the run inputs. The bot worked by coincidence, because
   `--var reviewer=on` populates both. Replaced with `{{vars.reviewer}}`, which
   says what is meant. **The compiler and the runtime disagreeing about what
   `input` means is an engine issue, not a bot one** — worth a ticket.

### Engine hardening

- Nothing committed to `pkg/` from this run. Two candidate tickets above
  (unknown-tool diagnostic; `{{input.x}}` edge-vs-compile divergence).
- Related and already filed: **#476** — grok and kimi cannot enforce
  `permission:`, which is why Copi's model ladder runs on claw rather than the
  CLI forfaits an operator would prefer.

### Lessons for next run

- **The reviewer must not share the answering model's family, and on this host
  that is not free.** `anthropic/…` on claw is *not usable* when the only
  Anthropic credential is the Claude Code OAuth forfait (`iterion models`
  reports every Anthropic row usable via claude_code alone). The reviewer's
  primary would have failed every turn and fallen through to the openai rung —
  Copi's own family — turning cross-review into a mirror, silently. It runs on
  `backend: "claude_code"` for that reason, which is legal because claude_code
  enforces the gate and the node is `session: fresh`.
- **A leading question is the test that matters.** "Explain X" produced an
  empty critique three times; "I'm going to do X, right?" is what made the
  reviewer speak. Any future evaluation of this feature should use the second
  shape.
- Measure the reviewer's cost against a real session before recommending it to
  anyone. Three turns is not a sample.
- **Do not put the run store in `/tmp` on this host** — it is a 16 GB tmpfs.
  An earlier studio in this same session filled it and took the machine's
  shell with it. See `HANDOFF-worktree-pool.md`.
