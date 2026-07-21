---
name: doc-mismatch-taxonomy
description: Working taxonomy and evidence conventions for adjudicating Doki v2 drift-manifest candidates.
---

# Documentation mismatch taxonomy

Use these classifications while adjudicating a manifest candidate. They are a
reasoning vocabulary for the single v2 campaign, not required fields in
`campaign_output`.

| Kind | Meaning | Typical evidence |
|---|---|---|
| `stale_command` | A documented command or invocation is no longer accepted. | Runtime `--help`, Cobra registration, command tests. |
| `wrong_signature` | A documented function, method, type, or field shape differs from code. | Current declaration and callers. |
| `dead_link` | A Markdown target or heading anchor does not resolve. | Target path plus GitHub-style heading slug. |
| `removed_file_ref` | Prose or code formatting names a path that no longer exists. | Filesystem search and replacement location, if any. |
| `stale_behavior_description` | Prose describes behavior different from the implementation. | Execution path, tests, defaults, or schema. |
| `outdated_example` | A snippet no longer parses, compiles, or matches the public API. | Parser/compiler/test output and current examples. |
| `wrong_default_value` | A documented default differs from the configured default. | Flag/variable declaration or defaulting code. |
| `obsolete_capability` | Documentation claims a removed or unshipped capability. | Current command/API/feature registration. |
| `wrong_directory_layout` | A documented repository or package map is stale. | Current tracked tree. |
| `comment_lies_about_function` | An opted-in Go comment contradicts its implementation. | Adjacent implementation and tests. |
| `undocumented_capability` | The optional code-surface scan finds a public CLI flag, command, or diagnostic absent from the scoped docs. | `scan_code_surface` result plus documentation search. |

The manifest itself emits candidate kinds such as `file_ref`, `md_link`,
`cli_command`, `cli_flag`, `diagnostic`, and `symbol_ref`, with status
`drifted` or `unverifiable`. A manifest kind describes how the anchor was
extracted; the taxonomy describes the semantic problem after verification.

## Evidence anchors

Make every rationale reproducible. Prefer one of these shapes:

- `symbol`: `path/to/file.go:Identifier` for a named declaration;
- `line_range`: `path/to/file.go:42-58` for an unnamed block;
- `removed`: the former path plus clear evidence that it was deleted or moved;
- `external`: a documentation path, heading, or URL with no source-code line
  semantics.

Do not invent an exact line or symbol. If the manifest evidence is ambiguous,
search the live tree and cite what actually resolved. A missing path is not a
symbol anchor, and a source line is not an external-link anchor.

## Priority guidance

- **High:** following the docs causes a wrong action, security mistake, failed
  command, or incompatible integration.
- **Medium:** architecture, behavior, or repository layout is materially stale
  but does not immediately cause a dangerous action.
- **Low:** a minor count, label, or dated qualifier is stale. Fix it alongside
  substantive work in the same document.

Prioritisation does not replace verification. A mechanically drifted CLI flag
is usually high signal; an unverifiable `symbol_ref` in prose is usually low
signal until context proves otherwise.

## Not a documentation fix

- wording or style that is already accurate;
- aspirational content clearly labelled as a proposal or historical record;
- a code bug where the document states the intended, still-valid contract;
- an ambiguity that cannot be resolved from current code, tests, or explicit
  repository policy.

For a code bug, leave the accurate document intact, set `is_code_bug=true`, and
hand off the finding. For a genuinely blocking ambiguity, set
`needs_human=true`; otherwise document the uncertainty in `summary` and keep
moving.
