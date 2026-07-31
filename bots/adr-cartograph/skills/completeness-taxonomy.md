---
name: completeness-taxonomy
description: The 8 enum-locked kinds of feature gap Adry recognises in the completeness audit. Required tag on every gap entry.
---

# Completeness audit taxonomy

The second half of an Adry run is a **completeness audit**: for each
in-flight feature the survey discovered, report what is fully
implemented vs what is missing/unfinished. Each gap entry MUST be
tagged with one of the 8 `gap_kind` enum values below. Hallucinating
a new kind fails schema validation and triggers iterion's
parse-fallback retry path — you will be re-invoked until you pick a
valid kind.

If a finding does not fit one of these 8 kinds, it is **not a gap for
this bot**. Either fit it into the taxonomy or drop it.

| `gap_kind` | Description | Example |
|---|---|---|
| `stub_only` | A function/method/handler exists but its body is a `return nil` / `return ""` / `panic("unimplemented")` placeholder, possibly with a TODO. The shape is wired; the substance is not. | A `func (s *Server) Cancel(runID)` whose body is `// TODO: implement\nreturn nil`. |
| `happy_path_only` | The feature works on the success path but has no error handling, no validation of inputs, no fallback for partial failures. | A workflow that calls `os.ReadFile` and `_ = err`-discards the error. |
| `no_error_handling` | A specific narrower flavour of `happy_path_only`: errors are CAUGHT but only logged-and-swallowed, never propagated to the caller. | `if err != nil { log.Println(err) }` with no `return err`. |
| `no_tests` | The feature has a `_test.go` (or equivalent test file) that is empty, has only a `TestSmoke` placeholder, or covers a different code path. The implementation is real but unproven. | `pkg/foo/foo_test.go` exists with just `func TestFoo(t *testing.T) {}`. |
| `partial_wiring` | The feature is implemented but only PARTIALLY wired into the system that consumes it (e.g. a CLI subcommand exists but isn't registered with the parent command; a handler is defined but no route maps to it). | `cmd/iterion/newcmd.go` defines `Use: "newcmd"` but no `rootCmd.AddCommand(newCmd)` exists. |
| `todo_marked` | The code carries a `TODO`, `FIXME`, `XXX`, or `HACK` marker on a load-bearing line that has not been resolved. NOT every TODO — only those marking incomplete work the feature depends on. | `// TODO: handle the retry case` inside the public-API method that's supposed to do retries. |
| `dead_end_branch` | A conditional branch exists (`if`, `switch case`, `match`) whose body is empty, comments-only, or returns an obvious sentinel without any real handling. | A `case ErrRateLimited: // ignored for now` clause that drops the error silently. |
| `unreachable_feature` | The implementation exists but no public API surface (CLI flag, public function, route, DSL keyword) actually reaches it. The feature has been written but cannot be invoked. | A new `pkg/foo/AdvancedMode` function with zero callers anywhere in the codebase. |

## Severity guidance

Independent of kind, each gap also carries a `severity` enum:

- `high` — the feature is **shipped but broken on real inputs**. A
  user invoking it as documented will hit the gap. Example: a
  `stub_only` CLI command that the README tells users to run.
- `medium` — the feature works for the common case but a documented
  edge case fails. Example: `happy_path_only` retry logic where the
  retry budget isn't honoured on context cancellation.
- `low` — the feature is internally inconsistent but no documented
  surface reaches the gap. Example: `unreachable_feature` code with
  no callers; harmless until someone wires it in.

Only `medium` and `high` gaps are routed to `type:feature-gap`
handoff issues. `low` gaps are mentioned in the relevant ADR's
"Consequences / Known gaps" section but not filed — they're not
worth the operator's queue.

## What is NOT a gap (do not raise)

- **Style preferences** — code you'd write differently but that is
  technically complete.
- **Missing documentation** — that's docs-refresh's job, not Adry's.
- **Mechanical refactors** — see `decision-vs-mechanic.md`. A
  function that could be cleaner is not a gap.
- **Missing features** — the feature has not been started yet.
  Adry audits IN-FLIGHT features; from-zero feature planning belongs
  to whats-next or feature_dev.
- **Code-side bugs** — a bug in a complete feature is a bug, not a
  gap. Set `is_code_bug: true` on the survey output if you must
  surface it, but Adry's downstream tooling won't act on it.

## The gap entry shape

Each entry in `survey_output.gaps[]` MUST carry:

```
feature:      short kebab-slug-able name of the feature
implemented:  ≤400 chars — what currently works (cite file:line)
missing:      ≤400 chars — what is incomplete (cite file:line)
gap_kind:     one of the 8 values above
severity:     low | medium | high
evidence:     ≤300 chars — the exact code excerpt or grep result
                proving the gap (the tool invocation that confirmed it)
```

Without `evidence` the gap is a façade. The campaign verifies cited gaps
before handing them off and dismisses anything the survey did not ground in
code.

## Output sizing

Cap the gap set at 30 entries per run. If the survey discovers more,
prioritise: `high` severity first, then `medium`, sort by feature
name. The remaining tail surfaces on subsequent runs as earlier gaps
are closed.

The cap keeps the survey context and potential board churn bounded; the tail
surfaces on later Adry runs after higher-priority gaps are closed.

## How gaps become handoff issues

`build_manifest` passes only `medium` / `high` gaps to the campaign. After
its last ADR commit, the campaign lists existing `inbox` issues to avoid
duplicates, then creates one inbox finding per new gap. The body carries the
`implemented` + `missing` + `evidence` triple; labels are `findings`,
`type:feature-gap`, `source:adr_cartograph`, and `severity:<level>`.

The campaign does not route the issue directly. A later triage can assign
`feature-gap-fill` when appropriate. A no-op run creates no handoff, and the
inbox preflight prevents repeated Adry passes from duplicating one.
