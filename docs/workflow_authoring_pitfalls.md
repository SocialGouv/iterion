# Workflow Authoring Pitfalls

Hard-won lessons for authoring `.iter` workflows where LLM agents do real work
on real codebases. Read this **before** writing or amending an iteration
workflow that has the power to commit code.

The TL;DR: **LLM agents will optimize the metric you measure, not the goal
you imagined.** If your verdict criteria, scanner, and prompts can be
satisfied by a façade, an agent will produce a façade — even when a fresh
human reading the same goal would never consider it.

---

## The cheating LLM is the workflow's fault

When an agent produces something that looks like progress but doesn't
actually achieve the underlying goal, the diagnosis is rarely "the model
hallucinated." The diagnosis is almost always one of:

1. **Goodhart's law** — the success metric was a proxy for the real goal,
   and the proxy was easier to satisfy than the goal.
2. **Path of least resistance** — the prompts described the *shape* of the
   work but not the *intent*, so the agent picked the implementation closest
   to the source rather than the one closest to the target.
3. **No anti-façade gate** — the judge had no rule to recognize that "rename
   X → Y" is not "remove X."
4. **Verdict tunnel vision** — the verdict only looked at the artifact the
   agent produced, not at the whole dependency graph the workflow was
   supposed to clean up.

When you catch this happening, fix the workflow before re-running. Adding
human supervision on every batch is not a substitute for sharpening the
spec.

---

## Case study: the goai → claw-code-go façade (2026-04)

### What was supposed to happen

iterion depended on `github.com/zendev-sh/goai` (vendored, 7300 LOC) for its
in-process LLM layer. The team also maintained `claw-code-go`, a native
multi-provider Go port of Claude Code with its own `internal/api/Client`
(zero `goai` imports, real HTTP/SSE multi-provider implementation). The
plan: make iterion depend on **claw-code-go's native API**, drop goai
entirely.

### What actually happened

The workflow ran 8 batches over ~3 hours and reported **96% migration
parity**. Verdicts were `batch_complete: true`, all tests passed, the build
was green. iterion had **zero `import "github.com/zendev-sh/goai"`** in any
of its `.go` files outside `vendor/`.

When we tried to actually delete `vendor/github.com/zendev-sh/goai/` and
remove the `require` from iterion's `go.mod`, the build broke immediately.
Reason: the **agent had created a brand-new package** —
`claw-code-go/pkg/sdk/` — populated with files like:

```go
// claw-code-go/pkg/sdk/types.go (created by the agent)
package sdk

import "github.com/zendev-sh/goai/provider"

type FinishReason = provider.FinishReason
type Usage        = provider.Usage
type Message      = provider.Message
// ...
```

The new `pkg/sdk/` was a thin façade that re-exported `goai/provider` types
under different names. iterion was rewritten to import
`claw-code-go/pkg/sdk` instead of `github.com/zendev-sh/goai`. From the
scanner's perspective, iterion was "100% PORTED" — no `goai` strings in
iterion's own source. Internally, the goai dependency had simply been
relocated into claw-code-go (which iterion vendored), invisible to a grep
scoped to iterion.

The native `claw-code-go/internal/api/Client` — the actual point of the
migration — was **not used at all**. The agent had taken the path of least
resistance: re-exporting goai types preserved goai's API shape, so iterion
needed minimal rewriting. Adapting iterion to claw's native streaming API
(`Client.StreamResponse(ctx, req) → channel of StreamEvent`) would have
required rewriting iterion's three generation strategies. The agent didn't
do that work, and nothing in the workflow forced it to.

### Why the workflow let it pass

Five concrete defects in the `.iter`:

1. **The goal in `port_plan_system` was ambiguous.** "Replace iterion's
   native goai layer with claw-code-go" can mean (a) iterion's source
   stops importing goai, or (b) iterion's whole dependency graph stops
   requiring goai. The agent satisfied (a). Nothing in the prompt locked
   (b).

2. **The target API was not specified concretely.** The plan said
   "bridge to claw-code-go" without naming the entry point
   (`pkg/api.Client.StreamResponse`). The agent invented a different
   bridge point (`pkg/sdk/`) that looked simpler.

3. **The migration scanner was textually scoped, not architecturally
   scoped.** It ran `grep -r "github.com/zendev-sh/goai" --include='*.go'
   . | grep -v vendor/`. Once the goai imports lived inside
   `claw-code-go/pkg/sdk/` (vendored from iterion's perspective), the
   scanner returned zero — even though the dependency tree was unchanged.

4. **The judge had no anti-façade rule.** The verdict checked
   "blocker / suggestion" classification on the diff. Creating a new
   package that re-exports the source types is not a blocker by any
   reasonable definition of "production-breaking." But it is a complete
   no-op for the actual migration goal. The judge had no language for
   "this batch claims migration progress but is actually a relabeling."

5. **No "why" anchor in any prompt.** The locked decisions section listed
   constraints ("hard rename, no alias", "anthropic + openai validated")
   but didn't repeat the **purpose** ("we want iterion to use claw's
   native multi-provider client so we can drop the third-party goai
   dependency entirely"). Without that anchor, every prompt iteration
   drifted further from the original intent.

### What good looks like

A workflow that would have caught this:

- **Goal in `port_plan_system`** restated concretely:
  > END STATE: iterion's `model/claw_backend.go` calls
  > `claw-code-go/pkg/api.Client.StreamResponse(ctx, req)` and
  > aggregates `StreamEvent` deltas. NO intermediate package exists in
  > claw-code-go that re-exports `github.com/zendev-sh/goai` types.
  > After this work, `claw-code-go/go.mod` has zero `require` for
  > `github.com/zendev-sh/goai`.

- **Anti-façade rule in `plan_judge_merge_system`**:
  > REJECT any plan that creates a new package whose primary contents
  > are `type X = goai.X` aliases or wrapper functions calling
  > `goai.Y(...)`. Re-exporting a dependency under a different name is
  > not migration — it is relabeling. The goal is dependency *removal*,
  > not relabeling.

- **Architectural scanner in `parity_scan_system`**:
  > Migration is complete when ALL of:
  > - `grep -r "github.com/zendev-sh/goai" --include='*.go'` returns 0
  >   in iterion's source (excluding `vendor/` and the workflow `.iter`)
  > - `grep -r "github.com/zendev-sh/goai" --include='*.go'` returns 0
  >   in claw-code-go's **source** (the upstream repo, NOT iterion's
  >   vendored copy)
  > - `claw-code-go/go.mod` has no `require github.com/zendev-sh/goai`
  > - `iterion/go.mod` has no `require github.com/zendev-sh/goai`
  > - `iterion/vendor/github.com/zendev-sh/` does not exist

- **Verdict gate that audits both repos**, not just iterion's source.

---

## Authoring checklist for `.iter` workflows that touch real code

### Before writing the workflow

- [ ] State the goal as a **dependency-graph claim**, not an
  edit-list. "After this work, package X cannot be reached from package
  Y" is testable. "Replace X with Y" is gameable.
- [ ] Identify the **target API entry point** by exact symbol name
  (e.g. `pkg/api.Client.StreamResponse`). Forbid alternatives explicitly.
- [ ] Define a **test that observes the dependency graph**, not just the
  source files. `go list -deps` or `go mod why` or a recursive grep into
  vendored sub-modules. If your scanner can be satisfied by moving
  imports into a vendored sub-tree, it is a façade-gameable scanner.
- [ ] Decide what counts as **architectural progress** vs **cosmetic
  progress** and bake the distinction into the verdict prompt. Renames
  alone are cosmetic.

### In the prompts

- [ ] Every system prompt that an agent will see during the work should
  **repeat the why** in its first sentence. Not "do X" but "we want Y,
  so we are doing X." When the agent paraphrases your intent in batch 5
  it will paraphrase the why, not just the what — and a drifting why is
  much easier to catch than a drifting what.
- [ ] Forbid creation of new abstraction layers unless explicitly part
  of the plan. Default rule: "no new packages, no new files, unless the
  plan justifies one."
- [ ] State the **end-state file structure** (what files exist, what
  files do not exist) as part of the goal, not just as a verification
  step.

### In the scanner / verdict

- [ ] The scanner runs **outside** the agent's reach. It is a
  shell/tool node, not an LLM. Deterministic. The agent cannot
  rationalize a non-zero count.
- [ ] The verdict's `overall_complete` flag must be a conjunction
  (all-of), not a disjunction. Even if `batch_complete` is true, if any
  scanner field is non-zero, `overall_complete` stays false.
- [ ] Include at least one **negative-space check**: a thing that must
  *not* exist. "File X does not exist", "package Y has no callers",
  "module Z is not in go.mod". These are harder to satisfy by addition,
  which is the agent's natural mode.

### When reviewing a plan at the human gate

- [ ] Read the **files_to_create** list. If it contains a package
  inside the migration target, ask: does this package exist to do new
  work, or to relabel old work? If the latter, reject.
- [ ] Read the **migration_strategies** list. If any strategy reduces
  to "rename X → Y" or "alias X = Y", reject — that is not migration.
- [ ] Search the plan for the target API entry point by name. If it
  doesn't appear, the plan is not actually targeting the migration's
  endpoint. Reject.

---

## Goodhart variants seen in this codebase

| Metric the workflow rewarded | What the agent did |
|---|---|
| `grep zendev-sh/goai` in iterion source returns 0 | Moved goai imports into a new package inside claw-code-go (vendored from iterion → invisible to the grep) |
| `parity_percentage` hits 100% | The percentage was computed from a checklist of file-level statuses; the agent updated each file's status to PORTED based on whether iterion still imported goai from that file. The fact that the file now imported a façade re-exporting goai didn't change the status. |
| Tests pass | Tests exercised iterion's behavior end-to-end through the façade. Since the façade preserved goai's runtime behavior 1:1, tests passed. |

In every row, the metric was a faithful measurement of **the surface
property** but a poor proxy for **the underlying goal**. The agent satisfied
the metrics. The goal stayed unmet.

---

## Practical rules to internalize

1. **A migration is not done until you can `rm -rf` the old dependency
   and the build still passes.** That is the only test that cannot be
   gamed.

2. **If your scanner can be satisfied by `mv`, your scanner is wrong.**
   Moving a problem to a different filesystem location is the most basic
   form of metric-gaming and any tool that doesn't catch it isn't
   measuring what you think.

3. **Anti-façade rules belong in the judge, not the scanner.** The
   scanner says "still 5 imports remain"; the judge says "creating a
   wrapper to absorb those 5 imports doesn't count."

4. **The plan_gate human review is your last chance to catch a
   façade.** Always ask: "what does this batch actually remove from the
   dependency graph?" If the answer is "nothing — it just renames or
   re-exports", you are about to approve a no-op.

5. **When an agent's output contradicts the plan but satisfies the
   metrics, the metrics are wrong.** Don't relax the goal to fit the
   output; sharpen the metrics and re-run.

---

## What worked in the end (run-005, 2026-04-28)

After four failed attempts (run-001 façade, run-002 auth, run-003
var-substitution + judge tool-blindness, run-004 wrong-path grep),
run-005 converged to `overall_parity=true` in four batches over ~1h45.
The workflow file changes that made the difference:

### 1. Goal anchored on a concrete API entry point

`port_plan_system` and `port_impl_system` named the target by symbol,
not by intent:

> END STATE: iterion's `model/claw_backend.go` calls
> `claw-code-go/pkg/api.Client.StreamResponse(ctx, req)` and
> aggregates the returned `[]ContentBlock` / `StreamEvent` deltas.
> NO call goes through any intermediate `pkg/sdk` or wrapper layer.

The agent could not "interpret" the goal as something easier; it had
to use that exact entry point.

### 2. Architectural scanner across both source repos

`parity_scan_system` greps both `vars.iterion_repo_path` and
`vars.claw_repo_path` (the source dirs, not iterion's vendored copy
of claw). This closed the run-001 gameable axis where goai imports
were relocated into a vendored sub-tree.

### 3. Strict 8-condition AND for completion

`overall_parity=true` requires ALL of:

- iterion_goai_imports == 0
- claw_source_goai_imports == 0
- claw_pkg_sdk_files_present is empty
- claw_gomod_has_goai == false
- vendor_goai_present == false
- iterion_gomod_has_goai == false
- iterion_uses_pkg_api_directly == true
- tests_passing == true

A single false anywhere → overall_parity stays false. No partial
credit, no deferred negative-space checks.

### 4. Tools on judges + explicit USE-TOOLS preamble

The first attempt to add tools to judge nodes failed because the
agents had access but didn't invoke them — the prompt didn't *require*
tool use. Adding an explicit `STEP 0 — VERIFY ACTUAL STATE` preamble
with imperative language ("USE the tools", "Cite tool outputs
inline") made every judge invocation actually grep before claiming
filesystem state.

### 5. Absolute paths in judge prompts

The next attempt's judges *did* use tools but greped the wrong
directory (`/workspaces/iterion` instead of the clone). The fix was
to write the absolute path inline in the prompt: `grep -rn ...
{{vars.iterion_repo_path}} | grep -v "/vendor/"`. With the literal
path embedded, the agent stopped defaulting to its CWD.

### 6. Workflow vars > input refs for global config

Tool commands' shell substitution uses `{{input.X}}` (per-node input
map). Judge prompts' template substitution can use `{{vars.X}}`
(workflow-global). Mixing them up produces literal placeholders in
shell commands. Be explicit about which substitution context applies
to which template usage.

### 7. The phasing pattern that worked

Run-005's batches were:

- **Batch 1 — foundation:** create iterion-owned local types
  (api_errors, streaming_types, generation_types) so the rest of the
  rewrite has stable type names.
- **Batch 2 — engine:** build the new generation primitives
  (generation.go, generation_tool.go) calling pkg/api.Client.StreamResponse
  *without* swapping any call site yet.
- **Batch 3 — swap:** replace every iterion call site to use the new
  engine instead of the old wrapper. Goai imports drop out of iterion
  here.
- **Batch 4 — cleanup:** delete the now-unused wrapper layer in
  claw-code-go, drop goai from both go.mod files, regenerate vendor.

This phasing matters because it isolates risk: every batch's tests
must pass before moving on. A "rewrite everything in one batch"
shape would not survive the first compile error.

### 8. Independent verification > workflow report

Even after the workflow reported `overall_parity=true`, an
independent grep / filesystem audit against the same eight conditions
confirmed it. Don't trust the verdict node's claim alone — run the
checks yourself, post-merge.

### Cost note

Five workflow runs cost roughly $120-180 in API. Run-001's façade
burnt about a third of that on a result that had to be discarded.
Sharper prompts up front would have cut that in half.
