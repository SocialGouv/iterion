---
name: fp-memory
description: |
  How to read and write `.sec-audit/fp-known.yaml` — the
  cross-run false-positive memory for sec-audit-source. Load this in
  the `triage` node (read-only) and the `revalidate` node (which may
  append or invalidate entries).
---

# FP memory — `.sec-audit/fp-known.yaml`

A committed, human-reviewable YAML file in the scanned repo that
records curated false positives. The bot reads it before LLM triage
and may append new entries during revalidate. Operators may edit it
by hand at any time.

## Location

```
<workspace_dir>/.sec-audit/fp-known.yaml
```

If `.sec-audit/` doesn't exist yet, the bot creates it. The file
itself is intentionally in the repo (NOT in `~/.iterion/`) because:

- FPs are repo-specific (a pattern that's an FP here may be a real
  vuln in another codebase),
- humans should be able to review + revert FP entries in PRs,
- a fresh clone of the repo should inherit FP knowledge.

## Schema

```yaml
known_false_positives:
  - id: fp-2026-001               # unique within file; auto-generated as fp-<YYYY>-<NNN>
    finding_type: ssrf            # MUST match [[finding-taxonomy]]
    file: "pkg/server/proxy.go"   # relative to workspace_dir
    line_range: [120, 145]        # inclusive; SUPERSET match counts
    matcher: "trivy-config-allowoutbound"  # original scanner+rule id
    rationale: |
      URL validated against static allowlist at
      pkg/policy/allowlist.go before any outbound call.
    confirmed_by: "@devthejo"
    confirmed_at: "2026-05-19"    # ISO date
    expires_at: null              # optional ISO date; entry ignored after
    fingerprint: "sha256:..."     # OPTIONAL — sha256 of the suppressed snippet
                                  # used to detect drift; see Invalidation below
```

## Match rules (triage)

A candidate matches an entry if **all** hold:

1. `candidate.finding_type == entry.finding_type`
2. `candidate.file == entry.file`
3. `candidate.line_range ⊆ entry.line_range` (candidate is within
   the suppressed range; a candidate that extends beyond does NOT
   match)
4. `candidate.matcher == entry.matcher` (exact, including
   scanner prefix)
5. `entry.expires_at == null OR now < entry.expires_at`

On match, the candidate's status flips to `known_fp` and it is NOT
sent to revalidate (saves tokens). It IS surfaced in the markdown
report under a "Suppressed (curated FPs)" section with link to the
entry id.

## Invalidation (revalidate)

The `revalidate` judge MAY mark a known-FP entry as `stale` when:

- The file no longer exists, OR
- The line range no longer contains the validating logic referenced
  in `rationale`, OR
- `fingerprint` is set and the snippet's sha256 no longer matches.

The judge then:
1. Re-promotes the underlying finding as a real candidate.
2. Appends `stale_since: <date>` and `stale_reason: "..."` to the
   entry (does NOT delete — preserves history).

## Append rules (revalidate)

When revalidate `dismiss`es a candidate with strong rationale, it
MAY append a new entry to `fp-known.yaml` (capability:
`file_edit` scoped to that exact path). Requirements:

- The rationale MUST cite the upstream control: function name,
  middleware, allowlist, etc. with a file:line anchor.
- `confirmed_by` is set to `judge:revalidate@<bot-version>`.
- `expires_at` is set to 90 days from `confirmed_at` for
  judge-appended entries (human-confirmed entries can have
  `expires_at: null`).

Never append on a `dismiss` whose rationale boils down to "scanner
mis-fires" or "false positive" without a specific control — those
suppress real signal and inflate trust.

## Editing by hand

Operators are encouraged to edit this file directly:
- Set `expires_at: null` to make an entry permanent.
- Adjust `line_range` when refactoring keeps the rationale valid.
- Delete an entry to force re-surfacing.

The bot ALWAYS re-reads the file at the start of every run; there's
no caching layer to invalidate.

## Why not just use the kanban "won't fix" state?

The board is the *current* state of findings; `fp-known.yaml` is
the *learned* knowledge that prevents re-creating the same issues
across runs. They serve different audiences:

- Board: "what's open right now"
- `fp-known.yaml`: "what we already decided isn't real"

A finding closed on the board with `won't-fix` does NOT
automatically suppress future surfacings — only an entry in
`fp-known.yaml` does.
