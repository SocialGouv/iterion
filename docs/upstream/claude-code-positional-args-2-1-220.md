# `$1`–`$9` in project commands resolve one position early — draft upstream report, not filed

Measured evidence for iterion issue #1713 across Claude Code 2.1.220 and 2.1.282: the CLI substitutes `$N` zero-based and did not change, and the current documentation now defines the same zero-based contract, so the doc contradiction the report was drafted against no longer exists and filing it is the operator's call.

<!--
  DRAFT upstream report for anthropics/claude-code — the deliverable of
  iterion issue #1713 ("report upstream"). NOT FILED: nothing on this page
  has been posted anywhere; filing is the operator's call.

  Status after the 2026-10-04 re-measure (CLI 2.1.282): the CLI behavior is
  UNCHANGED since 2.1.220, but the premise of the draft no longer holds —
  the current documentation defines `$N` as zero-based shorthand for
  `$ARGUMENTS[N]`, exactly matching the measured behavior. The
  documentation has been rewritten (slash commands are now documented as
  skills) and the sentence this report was written against ("use `$1`,
  `$2`, etc. to access arguments individually (like shell scripts)") is
  gone from the live docs and from the archived copies checked. As
  drafted, the report asserts a doc/implementation contradiction that no
  longer exists; filing it as a defect would be incorrect. Read "Status
  after the 2026-10-04 re-measure" at the bottom before doing anything
  with the draft below.
-->

---

## The draft as written against the 2.1.220-era documentation

*(the would-be issue body; see the re-measure note at its end and the
internal status section below it before considering it for filing.)*

### Summary

The documentation for custom slash commands describes positional argument
substitution as "use `$1`, `$2`, etc. to access arguments individually
(like shell scripts)" — in a shell, `$1` is the **first** argument. The
implementation resolves `$N` with a **zero-based** index behind the
one-based name, so `$1` yields the **second** argument and the last
argument is unreachable by name.

### Environment

- Claude Code 2.1.220 (original measurement), 2.1.282 (re-measure, identical result)
- macOS/Linux, project commands under `<workspace>/.claude/commands/`
- invocation: `claude -p "/probe-three alpha beta gamma" --output-format stream-json --verbose --setting-sources project`

### Reproduction

`<workspace>/.claude/commands/probe-three.md`:

```
Echo verbatim: [$1][$2][$3][$ARGUMENTS]
```

Then, in that workspace:

```
claude -p "/probe-three alpha beta gamma" --output-format stream-json --verbose --setting-sources project
```

Read the **expanded user message** from the session transcript
(`~/.claude/projects/<workspace-slug>/<session-id>.jsonl`) — not from the
model's paraphrase, which can mask the substitution. On 2.1.282 the
invocation surfaces to the model as a `Skill` tool call
(`{"skill": "probe-three", "args": "alpha beta gamma"}`), but the
transcript still records the expanded command body, and the substitution
engine is the same code in both versions.

### Measured result (transcript, verbatim)

The user message of the expansion (session `57e2157d-021c-42ff-a4eb-dae8803d5760`,
CLI 2.1.282, 2026-10-04):

```
Echo verbatim: [beta][gamma][$3][alpha beta gamma]
```

- `$1` → `beta` (the 2nd word)
- `$2` → `gamma` (the 3rd)
- `$3` → left literal (out of range — the 1st word is unreachable)
- `$ARGUMENTS` → `alpha beta gamma` (correct)

Identical to the 2.1.220 measurement.

### Expected vs actual

Expected (documented reading, "like shell scripts"): `[alpha][beta][gamma][alpha beta gamma]`.
Actual: `[beta][gamma][$3][alpha beta gamma]`.

### Reference implementation

Extracted from the 2.1.220 bundle (quoted in the original report) and
byte-equivalent in shape in the 2.1.282 binary — only the minified
identifiers differ:

```js
e = e.replace(/\$(\d+)(?!\w)/g, (B, G) => {
  let ge = parseInt(G, 10);
  if (b[ge] === void 0) return B;   // out of range stays literal
  return F = !0, h(b[ge]);          // F = "a placeholder consumed args"
})
```

with the argument array built by

```js
function Mcr(e) { if (!e || !e.trim()) return []; let n = Od(e); return n.length > 0 ? n : e.split(/\s+/).filter(Boolean) }
...
let b = Mcr(n),  // n = the argument string
```

`Mcr` produces a plain array — no leading dummy element — and the digits
captured from `$N` index it directly, so `$1` reads `b[1]`, the second
token. The sibling indexed form uses the same array the same way
(`e.replace(/\$ARGUMENTS\[(\d+)\]/g, (B, G) => ... b[parseInt(G, 10)] ...)`),
so `$1` behaves like `$ARGUMENTS[1]`, not like a shell's `$1`.

### Why it matters

- A command body written against the documented reading silently means
  something else: every positional reference shifts by one, and the last
  argument cannot be referenced by name at all (it only exists through
  `$ARGUMENTS`).
- Any other harness that implements the substitution from the
  documentation — as iterion's `claw` backend did — produces different
  expansions for the same command file, and nothing in the CLI marks the
  divergence.

### Re-measure note (2.1.282, 2026-10-04)

Same probe, same transcript reading, same expansion
(`[beta][gamma][$3][alpha beta gamma]`); the reference implementation is
unchanged in shape. Cost of the re-measure probe: $0.018.

---

## Status after the 2026-10-04 re-measure (iterion-internal — not part of the draft)

**The draft above should not be filed as-is.** The behavior did not
change; the *documented contract* did. Evidence:

- Live page, fetched 2026-10-04
  ([code.claude.com/docs/en/slash-commands](https://code.claude.com/docs/en/slash-commands)):
  the substitution table documents
  `$ARGUMENTS[N]` — "Access a specific argument by 0-based index, such as
  `$ARGUMENTS[0]` for the first argument" — and
  `$N` — "Shorthand for `$ARGUMENTS[N]`, such as `$0` for the first
  argument or `$1` for the second."
- Archived copies carry the same explicit zero-based wording:
  web.archive.org snapshot `20260731124821` of
  `docs.anthropic.com/en/docs/claude-code/slash-commands` and snapshot
  `20261001023659` of `code.claude.com/docs/en/slash-commands`. The
  sentence "use `$1`, `$2`, etc. to access arguments individually (like
  shell scripts)" appears in neither. `docs.anthropic.com` and
  `docs.claude.com` both 301-redirect to the `code.claude.com` page, so
  no legacy host still serves the old wording.
- Consequence: the zero-based behavior was already the documented
  contract in every copy of the docs checked around the 2.1.220
  measurement that produced iterion #1713. The ticket's quoted sentence
  ("like shell scripts") is absent from those copies — it predates them
  or lived on a page since removed; either way it is not in the current
  docs. There is no live doc/implementation contradiction to report; the
  CLI matches its current documentation.

What actually changed is iterion's position: `claw` implements the
one-based reading, which followed the CLI's *earlier* documentation and
is now the divergent side — off by one against both the current docs and
the CLI, and exactly what the `DynamicBodyForms` runtime warning exists
to name.

Options for the operator (Jo):

1. **Do not file** (recommended). Filing a defect whose premise the
   upstream docs contradict costs credibility; there is no defect to
   report. The only upstream-adjacent item worth considering is a small
   *doc-feedback note* (not a defect) asking for a migration callout for
   bodies written under the old "like shell scripts" wording, which
   silently changed meaning — strictly optional.
2. **Decide claw's side deliberately** (tracked by #1713): keep the
   one-based reading behind the `DynamicBodyForms` warning — the status
   quo, no silent divergence, bodies keep meaning what they meant — or
   flip `claw` to zero-based, which rewrites the meaning of every command
   body already written against `claw` and needs a breaking-change
   framing. Neither is a docs edit; claw-code-go is first-party.

Raw evidence (operator machine, outside the repo):
`~/.claude/plans/cc-positional-args-probe/` — the probe command file,
the stream-json output and the session transcript of the 2.1.282
re-measure.
