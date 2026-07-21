---
name: gap-vs-feature
description: When to use Fini (feature-gap-fill) vs Featurly (feature_dev), and the schema of a valid gap spec. Load when the operator (or Nexie) is deciding which bot to dispatch on a feature ticket.
---

# Fini vs Featurly — choosing the right completer

Both bots use one adaptive implementation campaign with verified
in-stride commits, a deterministic build/test gate, and a bounded
continuation loop. They differ on the SHAPE of the input and the
PRESERVATION discipline applied during implementation.

## Pick Featurly (`feature_dev`) when

- The work is greenfield: no existing partial implementation to
  preserve. The ticket reads "Add a /healthz endpoint" or "Build the
  CLI flag --branch-name" — the feature does not yet exist anywhere
  in the workspace.
- The ticket describes the desired end-state as a single feature with
  a clear, externally-visible "done" state. Featurly's campaign does
  its own exploration and maintains a living implementation plan; it doesn't
  need a structured what's-implemented / what's-missing input.
- The implementer is free to choose abstractions. Featurly explicitly
  authors ADRs for non-trivial decisions because it OWNS those
  decisions on a greenfield run.

## Pick Fini (`feature-gap-fill`) when

- A partial implementation already exists. The ticket reads "Finish
  the migration of pkg/foo to the new API" or "Wire up the handler
  for the route /bar that already has a stub in pkg/bar/api.go" — the
  feature is half-done, and the operator wants the missing half
  closed without re-architecting the working half.
- An ADR survey (Adry) produced a `type:feature-gap` issue whose body
  is a structured gap spec (`implemented:` / `missing:` / `evidence:`).
  Fini consumes that body verbatim as `gap_spec`.
- Preservation matters. The half-done work shipped to a branch
  someone else owns, or has consumers that the operator can't break.
  Fini's campaign is gap-aware: it layers the missing parts on top
  of the existing seams instead of proposing a redesign.

## What a valid gap spec looks like

Fini parses prose, not strict JSON, so the spec is forgiving — but
every operator-facing or Adry-emitted gap spec should hit these three
sections:

```
implemented:
  - pkg/foo/api.go: Route surface + request/response types (lines 1-80)
  - pkg/foo/types.go: Foo, FooRequest, FooResponse
  - pkg/foo/api_test.go: Round-trip tests for the marshal/unmarshal

missing:
  - pkg/foo/handler.go: HandleFoo function wiring the route to a
    backing store (referenced at pkg/foo/api.go:42)
  - pkg/foo/handler_test.go: Table-driven tests for HandleFoo,
    covering the validation + happy path + error path
  - cmd/server/main.go: register Foo route on the server's mux

evidence:
  - pkg/foo/api.go:42  → declares route, calls undefined HandleFoo
  - cmd/server/main.go:88 → mux registration block (insert here)
  - docs/foo.md → feature description authored by the spec owner
```

The `evidence:` block is what makes Fini fast. Without it, the survey
node has to grep around to find the seams; with it, the survey reads
exactly the files it needs and emits a tight `existing_state`. Adry
runs that produce gap specs include evidence by default.

## Composition with Adry

The intended flow:

1. Adry surveys the codebase + docs, identifies a feature whose
   implementation is partial, and emits a `type:feature-gap` board
   issue with a structured gap spec in the body.
2. The dispatcher (or Nexie) routes the issue to Fini. Fini's
   `dispatch_vars.gap_spec` template renders the issue title + body
   into the `gap_spec` var.
3. Fini runs one adaptive gap-closing campaign, verifying and committing
   each missing item with a `Bot: feature-gap-fill` trailer. ADR-worthy
   decisions surfaced during the run are captured as `kind:improvement`
   inbox findings (and noted in the campaign summary) so the next Adry
   run picks them up.

This composition keeps each bot focused: Adry owns the survey + ADR
trail; Fini owns the gap-closing implementation; neither steps on the
other.
