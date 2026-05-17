---
name: doc-mismatch-taxonomy
description: The 10 enum-locked kinds of doc/code mismatch the doc-align bot recognises. Required tag on every blocker.
---

# Doc mismatch taxonomy

Every blocker raised by a doc-align reviewer MUST be tagged with one
of these 10 `mismatch_kind` enum values. Hallucinating a new kind
fails schema validation and triggers iterion's parse-fallback retry
path — you will be re-invoked until you pick a valid kind.

If a finding does not fit one of these 10 kinds, it is **not a
blocker for this bot**. Either fit it into the taxonomy or drop it.

| `mismatch_kind` | Description | Example |
|---|---|---|
| `stale_command` | A CLI command, flag, or shell invocation in docs differs from what the code accepts now. | README says `iterion run --foo`, but `--foo` was renamed to `--scope` in [pkg/cli/run.go](pkg/cli/run.go). |
| `wrong_signature` | A function / method / struct field signature shown in docs or a comment differs from the actual code. | Docstring claims `func Foo(ctx, name string)`, code is `func Foo(ctx, name, region string)`. |
| `dead_link` | A markdown link points to a path or anchor that no longer exists. | `[see resume docs](docs/resume.md)` but `docs/resume.md` was renamed/removed. |
| `removed_file_ref` | Docs reference a file or directory by path that no longer exists, outside markdown link syntax. | README mentions "see `cmd/legacy/main.go` for the old entrypoint" but `cmd/legacy/` was deleted. |
| `stale_behavior_description` | Prose claim about behaviour does not match what the code actually does. | doc says "merges into HEAD by default", code now requires `--merge-into=current` and skips otherwise. |
| `outdated_example` | A code block / example in docs fails to parse, compile, or correspond to the current API surface. | An `.iter` snippet in `docs/grammar/` uses a property that the parser no longer accepts. |
| `wrong_default_value` | Docs claim a default value different from the code's actual default. | docs say `timeout=30s default`, code default is `60s`. |
| `obsolete_capability` | Docs list a feature or capability the code no longer supports (or has not yet shipped). | README lists `--web-ui` as a flag, the web UI was extracted to a separate binary. |
| `wrong_directory_layout` | A directory / package tree shown in docs (e.g. ASCII tree, table of `pkg/` contents) is incomplete or misaligned with the filesystem. | CLAUDE.md's `pkg/` breakdown omits `pkg/conductor/native/` which exists on disk. |
| `comment_lies_about_function` | A Go (`//` or `/* */`) comment on a function, method, or package misstates what the code does. Only applies when `go_comment_globs` is non-empty. | `// fetchUser loads a user by ID` but the function loads users by email, not ID. |

## Severity guidance

Independent of kind, blockers also carry a `severity` enum:

- `high` — actively misleading: someone reading the doc and acting
  on it would do the wrong thing (e.g. run a removed command and
  get a confusing error, or follow a wrong example).
- `med` — out of date but unlikely to cause action errors (e.g.
  a tree diagram missing a subdirectory).
- `low` — minor staleness (e.g. an outdated count, "as of 2024"
  marker, etc.). Generally not worth fixing alone; bundle with
  higher-severity work on the same file.

## What is NOT a doc mismatch (do not raise)

- **Style preferences** — wording you'd phrase differently but
  that is technically accurate.
- **Missing documentation** — sections you wish existed. The bot's
  job is alignment, not authoring.
- **Code-side issues** — the bug is in the code, not the doc. Set
  `is_code_bug=true` on the blocker and escalate via `ask_user`;
  do NOT raise it as a fixable mismatch.
- **Spec ambiguity** — when neither doc nor code is clearly
  correct. Escalate via `ask_user`.

## Anchor classification (`anchor_kind` — new in v0.2.0)

Every blocker also carries an `anchor_kind` enum that classifies
the precision of the `code_anchor` citation. This makes the G3
round-trip (next-iteration reviewer re-greps the cited anchor)
mechanically auditable.

| `anchor_kind` | When to use | `code_anchor` shape |
|---|---|---|
| `symbol` | The mismatch is about a named function / type / const / var. The anchor identifies the symbol the reviewer can grep for. | `"pkg/foo/bar.go:Handler"` or `"pkg/foo/bar.go:func Handler"` |
| `line_range` | The mismatch is about a block of code without a clean symbol name (e.g. a config snippet, an inline expression). | `"pkg/foo/bar.go:42-58"` |
| `removed` | The doc claims something exists but it no longer does. The anchor names the former location. | `"pkg/legacy/old.go (removed in commit X)"` or `"<no longer exists>"` |
| `external` | The mismatch is about a non-code claim — e.g. a dead link to a doc file, a stale upstream URL, a wrong directory layout. The anchor is the filesystem/external path the doc references. | `"docs/old-thing.md"` or `"examples/preview_url_demo.iter"` |

Hallucinating a kind outside the enum fails schema validation and
triggers iterion's parse_fallback retry path — pick from the
four values above.

## Output shape (reminder)

Every blocker in `verdict_output.blockers[]` must include:

```
doc_path:        repo-relative path (must be in scan_docs.doc_files)
doc_line_start:  int
doc_line_end:    int
doc_excerpt:     ≤300 chars, exact quote
code_anchor:     string per anchor_kind table above
anchor_kind:     symbol | line_range | removed | external
code_state:      ≤300 chars, what the code actually does now
mismatch_kind:   one of the 10 values above
severity:        low | med | high
suggested_fix:   ≤400 chars proposed new doc text
is_code_bug:     true iff the DOC is correct and the CODE is wrong
```
