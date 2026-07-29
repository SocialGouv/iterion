# Review-&-merge gate (`interaction: review`)

A **review-&-merge gate** is a human node that walks the operator through
*testing* a change via a continuous **companion↔human dialogue**, then
**squash-merges the run's worktree during the pause** once the operator is
satisfied. It is the moment a run stops being autonomous and asks a human to
verify the work before it lands — the iterion equivalent of a GitHub
"squash and merge" review, with an agent guiding the test.

It is a fourth `interaction:` mode on `human` nodes, alongside `human`,
`llm`, and `llm_or_human`.

## At a glance

```iter
human ship_review:
  interaction: review
  model: "anthropic/claude-sonnet-4-6"   # the companion (writes test steps + verdict)
  system: companion_system               # the companion's contract prompt
  output: review_verdict                 # decision / confidence / blockers — routes downstream
  review_url: "{{outputs.provision.url}}" # optional env to open & test (studio Browser pane)
  posture: human_required                # human_required (default) | agent_verdict_ok
  merge_strategy: squash                 # squash (default) | merge
  merge_into: current                    # current (default) | none | <branch>
  max_turns: 8                           # dialogue asymptote backstop

workflow ship:
  entry: implement
  worktree: auto                         # REQUIRED — the gate merges this worktree
  implement -> ship_review
  ship_review -> done     when "decision == 'approved'"
  ship_review -> implement when "decision == 'changes_requested'" as fix_loop(5)
  ship_review -> fail                    # default fallback
```

Reference workflow: [examples/review-merge-gate.bot](../examples/review-merge-gate.bot).

## How it works

1. **First turn.** When the run reaches the gate, the **companion** (an LLM
   judge-style call, no tools) is shown a bounded diff of the run's commits
   and produces a short human-facing instruction: the action first, at most
   three checks, no more than 120 words (with an 800-character schema cap).
   The run **pauses**.
2. **Dialogue.** The studio renders the companion's message, the dialogue
   thread, an optional "open review environment" link, a reply box, and the
   merge controls. The operator can:
   - **reply** — the companion re-evaluates and asks a follow-up (re-pause);
   - **Approve & merge** — squash-merge the worktree and finish;
   - **Force-merge** — merge *without* the companion's verdict (a GitHub
     admin-merge; git safety guards still apply);
   - **Request changes** — route the `changes_requested` edge (e.g. back to
     the implementer).
3. **Merge during the pause.** Approve/Force squash-merges the run's
   worktree into the target branch *while the run is paused*, records
   `final_commit` / `final_branch` / `merged_into` / `merge_status=merged`
   on `run.json`, then advances to the terminal node. The run-end finalize is
   idempotent (it skips when the gate already finalized).

The dialogue is bounded by `max_turns` and trends to a verdict, so the gate
**converges to an asymptote** rather than looping (the rule every iterion
review loop follows).

## Posture — "human in the loop" toggle

- `posture: human_required` (default) — the gate always waits for the
  operator's explicit action, even when the companion is satisfied. The
  companion's verdict is shown but never auto-merges.
- `posture: agent_verdict_ok` — a **high-confidence** companion `approved`
  verdict auto-merges without a human click. This is "human-in-the-loop
  *off*": the agent's judgement is trusted to ship.

## Merge configuration

| field | values | meaning |
|-------|--------|---------|
| `merge_strategy` | `squash` (default) \| `merge` | squash collapses the run's commits into one; merge fast-forwards |
| `merge_into` | `current` (default) \| `none` \| `<branch>` | target branch; `none` = create the storage branch only (branch-only review) |

The studio merge form can override the strategy and the squash commit
message per-merge. These reuse the same primitives as the post-run
deferred-merge (`runtime.PerformDeferredMerge`), so the guards are identical:
the target must be the currently-checked-out branch and the working tree must
be clean, else the merge is rejected (the run becomes resumable so the
operator can fix the tree and re-approve).

## Provisioning a review environment (optional)

The `review_url` is a template (e.g. `{{outputs.provision.url}}`) resolved at
pause time. Pair the gate with an upstream agent that deploys/serves the
change and reports a URL — the gate links it into the studio Browser pane so
the operator can actually exercise the app. The provisioning step is entirely
**custom-prompt-driven** and optional; when `review_url` resolves empty the
link is hidden. iterion does not start or expose ports for you — the agent
reports whatever URL the operator should open (a cluster endpoint, a host
dev-server, a desktop build path).

## Diagnostics

- **C100** (error) — `interaction: review` without `worktree: auto`. A review
  gate squash-merges the run's worktree; without one there is nothing to merge.
- **C101** (warning) — `review_url` references an output of a node that does
  not exist. The URL simply renders empty at runtime.

A review gate also requires a companion `model` and an `output` schema (the
verdict), the same requirement as `llm` / `llm_or_human` modes.

## Persistence

The dialogue lives on one `Interaction` per logical execution of the review
node, as an ordered `turns` array; each round appends a turn and re-pauses with
the same ID, so the whole thread re-renders verbatim on resume. Outside a loop
(and on the first execution) the historical `<run>_<node>` ID is retained. A
later loop execution gains its iteration suffix; nested loops encode the full
iteration path so two logical reviews cannot share a thread. Resume uses the
interaction ID stored in the checkpoint rather than recomputing it, preserving
runs paused by older binaries. Events: `review_turn`, `review_verdict`,
`review_merged` (see [docs/persisted-formats.md](persisted-formats.md)). The
merge outcome is the standard `final_*` / `merge_status` fields on `run.json`
— the same ones the studio RunHeader and CommitsPanel already surface.

## Resume protocol (CLI / API)

The operator's action rides the resume `answers` map under reserved keys, so
no new endpoint is needed — `POST /api/runs/{id}/resume` and
`iterion resume --answer` both work:

| key | values |
|-----|--------|
| `__review_action` | `reply` \| `approve_merge` \| `force_merge` \| `request_changes` |
| `__review_reply` | free text (for `reply`) |
| `__review_message` | optional squash commit message override |
| `__review_merge_strategy` | optional `squash` \| `merge` override |

Example (force-merge from the CLI):

```sh
iterion resume --run-id <id> --file ship.bot \
  --answer __review_action=force_merge
```
