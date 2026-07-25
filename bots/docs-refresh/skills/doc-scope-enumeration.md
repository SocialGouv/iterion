---
name: doc-scope-enumeration
description: Contract for Doki's deterministic documentation footprint, incremental base detection, advisory hints, and writeable set.
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
doc_count / no_docs          footprint size (zero → the campaign authors an initial set itself)
mode / incremental_base      run mode; the resolved incremental diff base
recently_changed_code_files  prioritisation hint (code changed since incremental_base)
hints / hints_note           the advisory report (see the playbook)
```

The exact schema in `main.bot` is authoritative.

## Footprint contract

- The doc footprint is what the deterministic enumeration produced — do not
  silently omit inconvenient files. You MAY create new `.md` files (they
  are inside the writeable set and join the footprint on the next pass).
- `recently_changed_code_files` only helps prioritise. It never narrows the
  documentation or code-verification scope.
- In `incremental` mode the diff base is auto-detected from the newest
  `Bot: docs-refresh` commit trailer (or pinned via `diff_since`); on the
  first run, with no such trailer, the base stays empty and the pass behaves
  like a full sweep.

When the initial footprint is empty there is no separate bootstrap node: the
`campaign` agent authors a grounded initial set under `docs_dir` itself, and
those files join the footprint on the next scan.

## Campaign writeable set

The campaign may modify:

- Markdown files (`.md`) — existing docs and new ones it authors.

Do not modify code, configuration, generated files, or build files.
`scope_check` is the deterministic containment gate: it diffs the whole run
against its base and fails the pass on any out-of-scope path. Any
out-of-scope commit must be reverted on the next pass.

## Why enumeration is deterministic

An agent-selected audit set permits silent omissions. A tool-generated
footprint makes the doc universe reproducible and leaves scope selection
outside the agent's discretion — while everything downstream of it (what to
fix, what to write, what to dismiss) is the agent's judgment, checked by the
truth gates.
