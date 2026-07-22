---
name: doc-mismatch-taxonomy
description: Working taxonomy and evidence conventions for adjudicating Doki doc-drift issues (advisory hints and self-found).
---

# Documentation mismatch taxonomy

Use these classifications while adjudicating a documentation issue — whether
it came from an advisory hint or from your own survey. They are a reasoning
vocabulary for the single campaign, not required fields in `campaign_output`.

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
| `undocumented_capability` | A public capability, command, or code area is absent from the scoped docs. | Repo exploration (help output, entry points, an `unmentioned_area` hint) plus documentation search. |

The advisory scan emits hint kinds `missing_path`, `dead_link`,
`dead_anchor`, and `unmentioned_area`. A hint kind describes how the signal
was derived mechanically; the taxonomy describes the semantic problem after
YOUR verification — and most taxonomy rows (wrong defaults, stale behaviour,
outdated examples) have no hint kind at all: they are yours to find.

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

Prioritisation does not replace verification. A missing repo-rooted path or
a dead internal link is usually high signal; prose that merely looks like an
identifier is usually low signal until context proves otherwise.

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
