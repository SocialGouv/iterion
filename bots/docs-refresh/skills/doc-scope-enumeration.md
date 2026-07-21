---
name: doc-scope-enumeration
description: Contract for Doki's deterministic documentation footprint, bootstrap rescan, incremental hint, cache, and writable set.
---

# Documentation scope enumeration

`scan_docs` establishes the documentation footprint before the alignment
campaign. Enumeration is deterministic: configured `doc_globs` are resolved,
hard-excluded directories and `bundle_self_path` are removed, paths are sorted,
and hashes are recorded for the audit cache.

Conceptually, its output contains:

```text
doc_files                    sorted repository-relative Markdown paths
doc_count                    number of paths
footprint_hash               hash of the resolved footprint
scope_globs                  resolved configured globs
recently_changed_code_files  prioritisation hint from diff_since
pre_verified_docs            unchanged cached docs whose code refs are unchanged
noop_skip                    exact-HEAD clean-tree cache short-circuit
```

The exact schema in `main.bot` is authoritative.

## Footprint contract

- Treat `doc_files` as the complete and immutable audit footprint for the
  campaign. Do not silently omit inconvenient files or add unrelated ones.
- `recently_changed_code_files` only helps prioritise. It never narrows the
  documentation or code-verification scope.
- `pre_verified_docs` is mechanical cache evidence, not an agent assertion.
  `build_manifest` merges it with anchors verified against the live tree.
- A matching cached Git HEAD may produce `noop_skip=true` only for a clean tree
  with no explicit `issue_id`. Any requested or changed run proceeds normally.

There is one controlled exception to “scan once”: when the initial footprint is
empty, `author_docs` creates Markdown under `docs_dir` and the bounded
`author_rescan` loop runs `scan_docs` again. That second scan defines the
campaign's footprint.

## Campaign writable set

The campaign may modify:

- Markdown paths in the established documentation footprint;
- Go comments, and only comments, in paths matching `go_comment_globs` when the
  variable is non-empty.

Do not modify code bodies, configuration, generated files, or Markdown outside
the footprint. Although `scope_check` is a final deterministic containment
gate, the narrower footprint rule remains the campaign's authorisation
boundary. Any out-of-scope commit must be reverted on the next pass.

The audit cache is maintained by `update_audit_cache`, not by the campaign. It
records mechanically verified `doc::anchor` pairs after a successful run.

## Why enumeration is deterministic

An agent-selected audit set permits silent omissions. A tool-generated,
hashable footprint makes coverage reproducible and leaves scope selection
outside the agent's discretion. `footprint_hash` and the manifest counts are
the evidence to use when diagnosing a scope or cache discrepancy.
