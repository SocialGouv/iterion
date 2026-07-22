---
name: doc-scope-enumeration
description: Contract for Doki's deterministic documentation footprint, bootstrap rescan, incremental hint, noop cache, and writeable set.
---

# Documentation scope enumeration

`scan_hints` establishes the documentation footprint before each campaign
pass. Enumeration is deterministic: configured `doc_globs` are resolved,
hard-excluded directories and `bundle_self_path` are removed, and paths are
sorted. The same node then derives the ADVISORY hints report from that
footprint (see the playbook — hints are help, never scope).

Conceptually, its routing/telemetry output contains:

```text
doc_files                    sorted repository-relative Markdown paths
doc_count / no_docs          footprint size; zero routes to the bootstrap
recently_changed_code_files  prioritisation hint from diff_since
noop_skip / noop_reason      exact-HEAD clean-tree cache short-circuit
hints / hints_note           the advisory report (see the playbook)
```

The exact schema in `main.bot` is authoritative.

## Footprint contract

- The doc footprint is what the deterministic enumeration produced — do not
  silently omit inconvenient files. You MAY create new `.md` files (they
  are inside the writeable set and join the footprint on the next pass).
- `recently_changed_code_files` only helps prioritise. It never narrows the
  documentation or code-verification scope.
- A matching cached Git HEAD may produce `noop_skip=true` only for a clean
  tree with no explicit `issue_id`. Any requested or changed run proceeds
  normally.

There is one controlled exception to enumeration-then-campaign: when the
initial footprint is empty, `author_docs` creates Markdown under `docs_dir`
and the bounded `author_rescan` loop runs `scan_hints` again. That second
scan defines the campaign's footprint.

## Campaign writeable set

The campaign may modify:

- Markdown files (`.md`) — existing docs and new ones it authors;
- Go comments, and only comments, in paths matching `go_comment_globs` when
  the variable is non-empty.

Do not modify code bodies, configuration, generated files, or build files.
`scope_check` is the deterministic containment gate: it diffs the whole run
against its base and fails the pass on any out-of-scope path. Any
out-of-scope commit must be reverted on the next pass.

The noop cache is maintained by `update_audit_cache`, not by the campaign.
It records only the git HEAD the docs were aligned to.

## Why enumeration is deterministic

An agent-selected audit set permits silent omissions. A tool-generated
footprint makes the doc universe reproducible and leaves scope selection
outside the agent's discretion — while everything downstream of it (what to
fix, what to write, what to dismiss) is the agent's judgment, checked by the
truth gates.
