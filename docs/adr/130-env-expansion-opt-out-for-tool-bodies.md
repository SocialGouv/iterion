# ADR-130 — `env_expansion`: a tool body the engine leaves verbatim, and the lint where its expansion defeats the author

A tool node's body gains a property that tells the engine to stand back from
its braced environment forms, and a lint that names the bodies where the
engine's answering defeats an assignment the body makes itself.

- **Status**: Proposed
- **Date**: 2026-10-12
- **Deciders**: jo (direction: the authored body must be able to BE what executes, and the gap must be visible before a run), Claude (measurements, analysis)
- Relates to:
  - issue #1522 (this record), #1322, #1475; PR #1473 (`4c5bf4463c`), the
    per-bot repair this record generalises
  - [093-bot-vars-runtime-settings.md](093-bot-vars-runtime-settings.md) — the
    overlay `lookupToolEnv` resolves a body's `${ITERION_*:-default}` through;
    that dial stays, and stays silent
  - [044-adaptive-recovery-for-deterministic-action-nodes.md](044-adaptive-recovery-for-deterministic-action-nodes.md)
    — the `postcondition:` body is the third expansion site
  - [075-irref-fallback-for-oversized-cloud-queue-ir.md](075-irref-fallback-for-oversized-cloud-queue-ir.md)
    — the compiled AST is what crosses to a runner, so the property is wire state
  - [`docs/references/diagnostics.md`](../references/diagnostics.md) — the lint
    doctrine this record is held to: "a lint that misses is acceptable, a lint
    that lies is not"
  - issue #1607, `ir.ResolveVarTextAsWritten` / `pkg/cli/validate.go:992` —
    the as-written reading of a launch value ("the shell's own environment is
    not consulted, so the verdict and the walk are the same on a laptop and in
    CI"), which this record extends from launch values to tool bodies
  - [`docs/dsl.md`](../dsl.md), [`docs/workflow_authoring_pitfalls.md`](../workflow_authoring_pitfalls.md)
    — the two pages that gain the rule

## Context

A tool node's executable body is rewritten by the engine before any shell sees
it. `expandBracedEnv` (`pkg/backend/model/executor_tool.go:1253`) runs at three
sites, each immediately before the template resolver:

| body | site |
|---|---|
| `command:` | `pkg/backend/model/executor_tool.go:431` (then `resolveCommandTemplate`, then `bash -c`) |
| `script:` | `pkg/backend/model/executor_tool.go:601` — **every** language, `js` and `py` included |
| `postcondition:` | `pkg/backend/model/executor_verified_action.go:192` |

The rule, read on this tree:

- `${NAME:-default}` expands **always** — `bracedEnvWouldExpand` returns true on
  any `:-` (`executor_tool.go:1338-1341`).
- `${NAME}` expands whenever `lookupToolEnv` finds the name
  (`executor_tool.go:1359-1369`): the `ITERION_*` overlay first (`093-bot-vars-runtime-settings.md`), then
  the **engine process** environment, gated by `ir.ProcessEnvReadable`.
- Anything that is not `NAME` or `NAME:-default` passes through verbatim —
  `looksLikeEnvRef` (`executor_tool.go:1377-1399`) refuses `${NAME#x}`,
  `${NAME:+x}`, `${p.name}`.

So a value the body sets **itself** never reaches its own braced read: the
engine already answered, from an environment that is not the body's. The
expand-before-substitute ORDER is a deliberate injection defence
(`executor_tool.go:416-430`): expanding after substitution would re-arm shell
metacharacters inside values `shellEscape` had made inert. That order is not
negotiable here.

Six measured facts frame the decision. Everything below was read on
`feat/dsl-expand-env-hatch` at `8515a94f5` (rebased onto `v3.273.0`).

**F1 — the production witness.** `bots/sec-audit-source`'s run-id guard read
`${RUN_ID:-}`. The engine resolved it to `""` (RUN_ID unset in the engine
process) before `bash -c`, so the guard refused **every production deep-scan
pass from 16/09 to 21/09** while every harness test stayed green — the harness
substitutes `{{…}}` refs plainly and never had the engine rule. Repaired on
2026-09-21 by PR #1473, inside that one bot, guarded by one per-file test
(`bots/sec_audit_shell_safe_test.go:124`, which forbids *any* braced form in
that bot's `command:` blocks and reads no other file).

**F2 — the class is live.** Scanning the catalogue under the measurement's
glob — 169 of the 180 tracked `.bot` files, re-verified at `6e8f8a088` — for a
shell-bound body that BOTH assigns a name and reads it in a form the engine
expands: **5 reads, 3 bodies, 2 files.**

| site | read | mechanism |
|---|---|---|
| `bots/secured-renovacy/main.bot:4038` | `${COUNT}` after `COUNT=$(… jq 'length' …)` | expands if the engine process carries `COUNT` |
| `bots/secured-renovacy/main.bot:4471` | `${FAMILY}`, `${COUNT}` after both assignments | idem |
| `bots/adr-cartograph/main.bot:899` | `${PORT}` after `PORT=${ITERION_DISPATCHER_PORT:-4891}`, `${ID_SHORT}` after `ID_SHORT=${ID#native:}` | idem — a `PORT` in the engine's environment silently retargets the dispatcher transition |

All five are latent, not broken today: correct for exactly as long as the engine
process happens not to export the name. The triage reported three; the two
`adr-cartograph` reads are the measurement's own correction. The deliberate dial
read itself — `${ITERION_DISPATCHER_PORT:-4891}`, the dial of
`093-bot-vars-runtime-settings.md` — is **not** in the
set and must never be: the body never assigns that name.

**F3 — the non-shell arm is protected by a heuristic, not a contract.** Site
`:601` expands every `script:` language. What keeps a JS template literal
`${count}` alive is `looksLikeEnvRef && bracedEnvWouldExpand`: a lexical
identifier test plus "is the name set in the engine process". 17 distinct names
are read braced inside `language: js|py` bodies of the catalogue, among them
`bots/prod-watch/main.bot:3231`'s `${DB_PASSWORD}` — written as a placeholder
inside a Python comment, which the expansion does not see as a comment. An
engine process carrying `DB_PASSWORD` inlines the value into the script text
that `emitToolNodeFinish` records, which is precisely what `secretGuard`
materialises through env indirection to avoid (`executor_tool.go:453-462`).

**F4 — an author already paid for this and worked around it by hand.**
`bots/golden-master/main.bot:9859-9864` carries, in the bot, in capitals: "NO
`${...}` ANYWHERE in the emitted shell. The DSL expands environment expressions
— including the `${VAR:-default}` form — before the script runs, and a default
clause swallows everything up to the first `}`, which is the closing brace of
the very next template substitution." The mechanism is confirmed in the code:
the brace scan takes the **first** `}` (`strings.IndexByte`,
`executor_tool.go:1271`), so `${FOO:-{{input.x}}}` yields the default `{{input.x`
and a stray `}}`. That bot expresses every default as
`[ -z "$VAR" ] && VAR=…` because there was no way to ask the engine to stand
back.

**F5 — `validate --exec` does not reproduce the expansion at all.** The dry run
renders tool bodies through `model.RenderCommand` / `model.RenderScript`
(`pkg/backend/model/template_resolver.go:280,288`, called from
`pkg/dryrun/executor.go:419-435`), and neither calls `expandBracedEnv`. The
preview and the `bash -n` syntax check therefore run on text the engine will not
hand the shell.

**F6 — nothing names any of it at authoring time.** No code in
`pkg/dsl/ir/diagnostics_codes.go` covers it; `docs/references/diagnostics.md`
has no row; the closest the IR comes is a doc comment — `ExpandBracedWithDefault`
(`pkg/dsl/ir/validate_nodes.go:1242-1247`) explains the rule ("the author's
`$?`, `$1` and `$ec` belong to the shell, not to iterion") and emits nothing;
the whole public contract is one clause at `docs/dsl.md:309`
("Environment expressions use `${NAME}` (and supported default forms) before
execution"); `docs/workflow_authoring_pitfalls.md` says nothing. `ir.ToolNode`
(`pkg/dsl/ir/ir.go:368-421`) carries no field that could ask for anything else.

One structural fact this tree gained since the record was first drafted: the
linear `${…}` pass now exists in the IR too — `expandWithDefault` with its
`expandPolicy{bracedOnly, keepUnresolved}` (`validate_nodes.go:1292`), the
reading var values and fallback gates are held to. It resembles the engine's
rule but is **not** it: the engine consumes a segment up to the first `}` and
never sees a nested form, the IR pass resolves nested segments inside-out —
for `${A:-${B:-c}}` the engine consults `A` only and hands `${B:-c}` to the
shell, the IR pass consults both. The lint's reader predicate therefore
replays the ENGINE's segmentation through one exported helper (§3), and the
IR pass stays what it serves — value resolution for var readings.

## Decision drivers

- **Philosophy §1.** A bound with no override is a defect. The engine rewriting
  an author's body with no way to decline is such a bound; a load-bearing limit
  keeps a greppable escape hatch.
- **Philosophy §4, git-native first.** "the `.bot` as text … never degraded". A
  published `.bot` that cannot be read as what it executes is a degraded
  contract: F4 is an author discovering it the hard way.
- **Philosophy §2, the Nth-variant test.** The engine already distinguishes two
  expansion arms with different rules (`:-` unconditional, plain env-gated). A
  switch that cannot name the arms is the shape that forces an engine `if` next
  time.
- **The lint doctrine, in the repo's own words** (`diagnostics.md:158`):
  precision over recall. A check that reddens `PORT=${ITERION_DISPATCHER_PORT:-4891}`
  — documented, deliberate, the dial of `093-bot-vars-runtime-settings.md` —
  teaches authors to ignore diagnostics.
- **The order is a defence, not an ordering preference**
  (`executor_tool.go:416-430`): a decision may SKIP the expansion; it may never
  move it.

## Options considered

### A — the narrow lint alone (C188: the body assigns the name it reads braced)

- **Pros.** Measured precision: 5 hits in 169 files, 5 of 5 genuine, zero
  false alarms, and the documented dial stays clean. Reuses the existing seam —
  `validateTreeNoiseChannels` (`pkg/dsl/ir/validate_tree_noise.go:54-127`)
  already walks exactly the three bodies, already gates on shell-bound ones, and
  already owns `scanShellBody` (quote/comment/heredoc aware) and `bodyLineOf`.
  One PR, warning severity, so no catalogue bot stops compiling
  (`bots/catalog_parse_compile_test.go:39-40`); reversible by deleting one arm.
- **Cons.** The ticket asks for two things — "the authored text must stay what
  executes, **or** the gap must be visible at validate" — and A delivers only
  the second, for half the class. A body that reads a name it never assigns
  (`${DB_PASSWORD}`, `${HOME}`, a sandbox-provided name) stays silent, and the
  author who is told "rewrite it bare" has no answer when the braced form is
  what the downstream language needs (F3, F4).
- **Use case.** Wins the next sec-audit-shaped bug. Loses the day a body must
  hand `${VAR:-d}` to the shell verbatim: the lint's only remedy is a rewrite.

### B — the full-recall lint (warn on every engine-substitutable braced form, minus an allowlist)

- **Pros.** Makes the gap visible wherever it exists, the sandbox-env and
  host-env arms included. It is already the accepted rule for one bot
  (`bots/sec_audit_shell_safe_test.go:124` forbids every braced form there), so
  generalising it is a straight line from prior art.
- **Cons.** It lies on a correct line: `PORT=${ITERION_DISPATCHER_PORT:-4891}`
  is the engine dial `093-bot-vars-runtime-settings.md` built, and every future
  dial-in-body author
  inherits a permanent warning or a suppression. The allowlist (`ITERION_*`
  only? by name?) becomes a second contract to maintain, and the repo's own
  doctrine argues against it in its own words.
- **Use case.** Wins on the half A cannot see. Loses the first time an author
  legitimately reads the dial inside a body — which the catalogue does today.

### C — the escape hatch, plus A's lint — **chosen**

- **Pros.** The only option that honours §1 and §4 literally: the author takes
  the engine out of the way instead of rewording the body, and the lint finally
  has a remedy to name. Safe by construction: skipping an expansion does not
  move it, so the injection defence is untouched. It answers F3 and F4, which
  no lint can.
- **Cons.** A permanent public contract: a node property crosses the parser, the
  property registry and its generated artefacts, fmt/canon, the author twin, the
  cloud wire, the three executor sites and the dry run. Nothing in the catalogue
  *declares* it today (the 5 hits of F2 are all fixable by a bare-`$NAME`
  rewrite), so the property is bought for F3/F4 and for the bodies not yet
  written.
- **Use case.** Wins for a published `.bot` a third party must read as what it
  executes, and for a `language: js` body whose `${…}` is the language's, not
  iterion's. Loses nothing measured, costs an L.

### The `$${NAME}` literal escape, considered and rejected

A doubled-dollar escape (`$${NAME}` → a literal `${NAME}`, expansion skipped)
was weighed as a cheaper hatch: no property, no registry, no wire.

Rejected on three counts. It is **per-reference**, so a body with thirty JS
template literals (F3) needs thirty edits and the thirty-first is the regression
— the `.bot` would be *less* readable as what it executes, not more. It changes
the meaning of text that is legal today: `$${X}` is valid shell (`$$` is the
PID, followed by `{X}`), so the escape would silently rewrite a working body —
exactly the class this record exists to end. And it is invisible to `validate`:
there is nothing for a diagnostic to name, nothing for the dry run to report,
and no way for a reviewer to see at the node's header that this body's env
forms are its own. A node property is greppable, diagnosable, and reads in
one line.

## Decision

### 1. The property

```
tool <name>:
  command: |
    …
  env_expansion: none          # default: braced
```

- **Name and values: `env_expansion: braced | none`.** A noun naming *what*, with
  values naming the *form*, which is how the registry already speaks:
  `ambient_context: none|workspace|operator|all`, `sandbox: none|auto`,
  `interaction: none|…`. `none` is the registry's established word for "nothing".
  The working name `expand_env: off` was weighed and set aside: no property in
  the registry is a verb phrase with a switch value, `off` says what iterion
  stops doing rather than what the body contains, and — the Nth-variant test —
  an on/off switch cannot grow the value the engine's own two arms already
  imply (`braced_default`: honour `${NAME:-d}`, leave `${NAME}` alone), while an
  enum can.
- **Form `Ident` through `checked(…)`**, like `compress`, `policy`, `language`
  and `ambient_context`: the parser takes any word, the compiler names the code.
- **Default `braced` = today's behaviour**, exactly. An absent property is not a
  third state: the IR carries `""` and the engine reads it as `braced`.
- **Scope: the node, and all three of its bodies together.** A node has one
  recipe; a `postcondition:` that disagreed with its own `command:` about
  whether `${X}` is iterion's or the shell's is a trap, not a feature.
- **What answers a read under `none`, named.** Under `braced` the engine's
  chain answers: the `ITERION_*` overlay first, then the process environment
  behind `ir.ProcessEnvReadable` — the gate `platformcfg.CloudProcessEnvPolicy`
  arms in cloud. Under `none` no engine lookup happens: the body's reader is
  the tool process's own environment, which is not one thing — the curated
  sandbox environment for a sandboxed node, and the engine's full
  `os.Environ()` for a sandbox-opt-out node (`executor_tool.go:789` scripts,
  `:900` command bodies) — outside `ProcessEnvPolicy`'s reach, which polices
  the engine's expansion, not bash's. Two consequences the author accepts with
  the value: the ADR-093 overlay stops answering the body's `${ITERION_*}`
  reads (it is consulted at expansion time only, and the only overlay name
  exported into tool processes today is `ITERION_TREE_NOISE`,
  `treeNoiseEnvAppend`, `executor_tool.go:815`) — a body mixing a
  self-assignment with a dial read fixes the warning at the cost of the dial,
  and the docs say so beside the property. And a set-but-empty name changes
  answer without the text moving: the engine's presence semantics yield `""`,
  bash's own `:-` yields the default.
- **Refused where there is no body.** On an `action:` node the property is
  inert, which reads as configured. It is refused at compile with the arm and
  the code the identical case already uses — `language:` without `script:`
  (`pkg/dsl/ir/compile.go:1452-1453`, `DiagBadTemplateRef`). No code is minted
  for a refusal an existing arm already spells.
- **No workflow value, no run override, no environment variable.** Deliberate,
  and the one place this record declines a knob:
  - A run-level `none` would silently break every body that READS the dial
    (`${ITERION_DISPATCHER_PORT:-4891}`, `${ITERION_VIBE_EFFORT_CLAUDE:-high}`):
    the operator would be turning off a mechanism bodies depend on, from
    outside the file that depends on it.
  - §1's clause is "never silently replace an operator's explicit choice"; its
    mirror holds here. The author's override exists — it is the property. What
    is refused is a second, invisible channel that changes what a body *means*.
    An operator who needs a body's rule changed edits the `.bot`, which is §4.
  - Additive later: a workflow-level value is backward-compatible the day a bot
    wants the whole-file guarantee, and the precedence is written in advance —
    **node > workflow > default** — so the day it lands it adds a tier and
    changes no existing reading.

### 2. One chokepoint, pinned

The three executor sites stop calling `expandBracedEnv` and call one function
that takes the node, and the environment its caller speaks for:

```go
// ToolBodyEnv returns the body as its reader hands it on, and every braced
// read it processed, each with its arm — a plain `${NAME}` kept as written,
// or a `${NAME:-default}` answered from the lookup or resolved to its
// default. The body keeps only what survived; the list is the decision
// record the preview reports (4). `env_expansion: none` returns the body
// unchanged and an empty list — the env forms are declined, so the text the
// reader hands on is the authored text's own environment text. The decline
// is env-scoped: template substitution still runs after this call
// (`{{…}}` escaped, raw `{{!…}}`), and none never touches it. lookup
// is the environment the reader speaks for: the engine's own chain for a
// run, a lookup that answers nothing for a preview that must read the same
// on a laptop and in CI. A nil lookup answers nothing and is guarded here;
// it is never forwarded to a helper whose nil means the process environment.
func ToolBodyEnv(node *ir.ToolNode, body string, lookup func(string) (string, bool)) (string, []BracedEnvRead)
```

- Called at the **same position** as the expansion it replaces, so the
  expand-then-substitute order — the injection defence — is structurally
  preserved: with `none` the function returns its argument and nothing else
  moves.
- The injected lookup is what lets `pkg/dryrun` share the function without
  inheriting the engine's environment (see 4).
- **The pin is a class test**, the idiom `pkg/backend/model/claw_sandbox_env_class_test.go`
  already establishes for this package: the set of non-test callers of
  `expandBracedEnv` must be exactly `{toolBodyEnv}`. A fourth executor site
  that forgets the property fails a test instead of shipping a body the author
  asked to keep verbatim and the engine rewrote anyway. This is the
  "one throttle point, not N repeated guards" shape — the lesson the three
  sites of F1 cost once already.
- **Not governed: text the author did not write.** The recovery ladder's
  self-repair rung runs an LLM-proposed command
  (`executor_verified_action.go:267-276`), which is not authored text and is
  correctly expanded by nobody today. The property is about the author's bytes;
  that stays true.

### 3. The lint — one new warning code, C188

A new arm beside the tree-noise family, in its own file
`pkg/dsl/ir/validate_braced_env_body.go`, reusing the walk, the shell-bound
gate, `scanShellBody` and `bodyLineOf`:

> **C188** (warning) — a shell-bound tool body both **assigns** a name and
> **reads** it in a braced form the engine expands. The engine answers the read
> from its own environment before `bash -c` ever sets the value, so the
> assignment is dead.

- **Allocation.** C188 is the working code. The vars-subscript warning
  (#2481, PR #2570) claimed the same number in flight on 2026-10-20; if it
  merges first, this lint takes the next free code (C189) and nothing else in
  this record changes — the code is referenced from `diagnostics_codes.go`,
  `diag_catalog.go`, `docs/references/diagnostics.md` and the tests only.
- **Exactly one code is minted.** The invalid-value refusal below rides the
  arm `language:` already uses, so the C1xx band — of which this record
  consumes the last free slot — is not asked for a second.
- **The reader predicate is the engine's own scan, not a lookalike.** The
  engine expands a segment up to the FIRST `}` (`strings.IndexByte`,
  `executor_tool.go:1271`): for `${A:-${B:-c}}` its body is `A:-${B:-c`, the
  name is `A`, and `B` is never consulted — the default reaches the shell
  intact. The IR's linear pass resolves nested segments inside-out
  (`expandWithDefault`, `validate_nodes.go:1292`), a different semantics on
  exactly these forms, so a probe through it records a read of `B` the engine
  never makes and the lint would lie on the nested line — the one thing
  `diagnostics.md:158` forbids. The read set therefore comes from one exported
  helper beside that pass, `ir.BracedEnvReads(body) []BracedEnvRead`, which
  replays the engine's own segmentation — first-`}` consumption, split at the
  first `:-`, the C-identifier test — and returns each read as its name plus
  whether it carried a `:-` default. No position rides on the record: the
  finding's line comes from the scanner half (`bodyLineOf`). The lint fires on
  the intersection: a name must be live for the shell
  (recorded by `scanShellBody`'s unquoted and double-quoted branches) AND
  consulted by the engine (`BracedEnvReads`). A read inside a comment, a
  single-quoted span or a heredoc body never fires, and the miss is the
  documented kind. `expandWithDefault` stays what it serves — value resolution
  for var readings — and is nobody's third scanner. A table test in
  `pkg/backend/model` pins the helper to `looksLikeEnvRef`'s and
  `expandBracedEnv`'s behaviour on the whole disagreement class, nested forms
  included; that is what keeps `ID_SHORT=${ID#native:}` silent on the `ID`
  read while naming the `${ID_SHORT}` read on the same line.
- **The writer predicate is judged through `scanShellBody`**: `NAME=`,
  `export NAME=`, `read NAME`, `for NAME in` — so a comment, a heredoc body or a
  single-quoted span never counts as an assignment.
- One finding per (body, name), with the body-relative line in the message (the
  AST keeps no per-property span).
- **Silent when the node declares `env_expansion: none`**: the hatch is the
  remedy, and a check that keeps firing after its own fix trains authors to
  ignore it.
- **FIX**: read the variable bare (`$NAME`) so the shell answers, or declare
  `env_expansion: none` to keep the body verbatim. The two halves land
  staged: the lint ships first and its row names the bare-`$NAME` rewrite
  only — a remedy that refuses to parse would be its own diagnostic — and the
  property joins the Fix when the property slice lands it.
- Severity warning, so no catalogue bot stops compiling; the five reads of F2
  are converted in the same change as the lint.
- **An invalid value is a compile ERROR, under the arm `language:` already
  uses.** `env_expansion: typo` is refused the way `language: lisp` is —
  `DiagBadTemplateRef` (`C004`), with the accepted values spelled in the
  message (`compile.go:1482` is the shape: "unsupported language %q (want one
  of: …)"). The substance the earlier draft gave a dedicated C189 survives
  intact: a typo must not read as the default, because here the default is
  "the engine rewrites your body" and the author wrote the line to stop
  exactly that — the refusal is an error and names `braced | none`. What
  changes is only which code carries it: the band holds one free slot, and
  minting a second code for a refusal whose sibling arm already spells it
  would spend the catalogue on the number, not the guarantee. The arm's own
  rows are amended to carry the new case: `C004`'s catalogue Fix and its
  `diagnostics.md` row describe only malformed `{{...}}` refs today, and an
  author sent there by `env_expansion: typo` would find nothing about enum
  values — one clause joins both rows (an unsupported enum value on
  `language:` / `env_expansion:` lands here too).

### 4. `validate --exec` tells the truth

`pkg/dryrun` honours the property — required, since it prepares tool bodies —
and it ends F5. It reads a body through the same function, with a lookup that
answers nothing:

- `${NAME:-default}` resolves to the default — the answer the run uses when
  the engine's environment does not carry the name. The engine consults its
  lookup FIRST on this form and the default is the fallback, so when the
  engine's process does carry the name the run executes that value while the
  preview shows the default. The preview cannot know the engine's process,
  and does not pretend to: the report below names the form.
- A plain `${NAME}` is kept **as written**. Whether the engine answers it
  depends on the engine's own process, which the validating process is not —
  substituting the laptop's value would make the verdict follow the laptop.
- `env_expansion: none` keeps the body's environment forms verbatim in the
  preview too (template substitution still renders `{{…}}` beside them), so
  the property is visible in the one place an author looks before paying for a
  run.

This is not a new rule: it is the reading `iterion validate` already gives a
launch value — `ir.ResolveVarTextAsWritten`, `pkg/cli/validate.go:992`,
"the shell's own environment is not consulted, so the verdict and the walk are
the same on a laptop and in CI" (#1607, merged the same day as this record).
Extending it from launch values to tool bodies is the consistent move; making
the dry run read the host environment would contradict a doctrine the repository
had just written down.

What is left over — the reads the preview cannot answer — is **reported**
rather than guessed: per body, both arms the engine's environment can decide,
marking which `${NAME:-default}` reads the preview resolved to their default
(the run may answer them from its environment) and which plain `${NAME}` reads
it kept as written — and the property as the way to decline. The report is an
observation under its own finding kind, outside the `--strict` failing
predicate: a deliberate dial read is the legitimate shape of exactly this
fact, and must not redden a gate.

That report is what makes C188's narrow recall acceptable rather than excused.
The half a compile-time check cannot judge without lying on the dial — a body
reading a name only the engine's environment supplies (`${DB_PASSWORD}`,
`${HOME}`, a sandbox-provided name) — becomes a preview **fact** ("the engine
will answer `${DB_PASSWORD}` here, from its environment") instead of a
compile-time **judgement**. Precision at compile time, recall in the preview.

### 5. Backend parity

A tool node has no `backend:`. Its body is executed by the engine's one executor
(`pkg/backend/model`, reached from `executor.go:1530`) whatever LLM backend the
run's agents use, so `claw` ↔ `claude_code` parity is structural here: there is
no second implementation to wire, and no typed refusal to write. The parity
statement this record owes is the one above about authored versus
model-proposed text, and it holds identically on both.

### 6. Cloud twin

The compiled AST is what crosses to a runner
(`queue.RunMessage.IRCompiled`, ADR-075), so the property is wire state:
`ast.ToolNodeDecl` gains the field and `pkg/dsl/ast/jsonenc.go` its mirror.
Forgetting it would mean a node that must run verbatim runs expanded on a
runner, and only there. The house already guards that class reflectively, and
the two sweeps watch different halves of it:
`TestEveryASTFieldHasAJSONCounterpart` (`pkg/dsl/ast/jsonenc_sweep_test.go`)
reads the mirror struct's tags, so it reddens on a field with no JSON
counterpart, while `TestEveryFieldSurvivesTheTransport`
(`jsonenc_fill_test.go`) fills a document and round-trips it, so it is the one
that reddens on a forgotten CONVERTER line in `toolToJSON` or `toolFromJSON` —
the likelier omission. Between them the cloud twin ships in the same change by
construction, not by discipline.

The wire carries a version, and this field is exactly the protocol's
additive-intent case (`pkg/queue/types.go:44-48`): any ADDITIVE field whose
omission changes operator intent — a knob the caller explicitly set, that a
stale runner would silently fall back from — is a BREAKING change, and bumps
`SchemaVersion`. A runner built before the property decodes `IRCompiled` with
unknown JSON keys ignored, drops it, and expands a body the author declared
verbatim — failing open, on a runner, only there. The property slice therefore
bumps `SchemaVersion` 25 → 26 with the history line that names this record,
on the precedent of v=21 — ADR-119's `ambient_context`, the same IR-carried
node-policy shape (`queue/types.go:137`) — and leaves `MinSchemaVersion`
alone, the rollout's default safe shape
([docs/cloud-queue-schema-rollout.md](../cloud-queue-schema-rollout.md)): a
stale runner parks the message with a delayed Nak and a resume recovers it,
which is transient by design, never a silent expansion.

The AST→IR compile arm has no reflective sweep of its own — the two above
watch the JSON transport — so the property slice adds one: the every-kind
fixture, parsed, compiled and reflected so that every declared tool property
surfaces on `ir.ToolNode`. The class closes, not this field only.

## Consequences

**What becomes true.**

- A `.bot` can declare that a body's environment forms reach the shell
  untouched. The decline is env-scoped: the body still passes through template
  substitution — `{{input.*}}` shell-escaped, `{{!…}}` raw, the documented
  injection boundary (`docs/dsl.md:309`) — which the property does not touch.
  `iterion validate --exec` shows those forms verbatim, resolves the
  `${X:-d}` default
  the run uses when the engine's environment lacks the name, and names — both
  arms — the reads the engine's environment can decide: the `:-` forms whose
  name the engine's process may supply, and the plain `${NAME}` reads it will
  answer. The gap F1 hid for five days is visible before a run, not after it.
- A `language: js` or `language: py` body can stop depending on a heuristic:
  `env_expansion: none` is the contract F3's seventeen template literals and
  F4's hand-written workaround were missing.
- Five latent reads in the catalogue become bare `$NAME` reads, and a sixth
  class — a body that assigns and reads back braced — reddens at authoring time
  for every bot written from now on.
- The per-bot guard `bots/sec_audit_shell_safe_test.go:124` stops being the only
  thing holding the class: it is generalised to the catalogue where the rule is
  right for every bot (an assignment read back braced), and stays per-bot where
  it is stricter on purpose (sec-audit-source forbids *any* braced form, with or
  without a local assignment — a bot-specific decision, not a catalogue rule).

**What becomes impossible.**

- Adding a fourth executor site for a tool body without honouring the property:
  the caller-set test on `expandBracedEnv` fails.
- Reading `env_expansion:` as "inherit" after a typo: the value is refused as
  an error (the `language:` arm, `C004`), never read as the default.
- A silent operator-side flip of a body's rule: there is no run override and no
  environment variable, by decision.
- Moving the expansion relative to the template substitution: the chokepoint
  sits where the expansion sat, and the injection invariant has its own witness.
- A `validate --exec` verdict that follows the validating machine's
  environment: the preview's lookup answers nothing, by construction.

**Migration and compatibility.**

- Fully backward-compatible. `braced` is today's behaviour and is what every
  existing bot gets; no `.bot` changes meaning, nothing is deprecated, no bundle
  needs a floor bump.
- C188 is a warning: no bot stops compiling, no gate turns red.
- A bundle carrying `env_expansion:` run by an **older** engine parses it as an
  unknown property and refuses the file loudly (**E012**,
  `DiagUnknownProperty`, `pkg/dsl/parser/diagnostic.go:26`), which is the
  honest direction for a property whose absence would re-enable the expansion
  the author declined. No bundle floor is pinned for it: nothing in
  `SyntaxFloors` asks for this property, `dsl migrate` would not propose it
  and `floorsalign` would not align it — the floor machinery earns its
  release-cut coupling when a syntax family needs the push-admission boundary,
  and until one does the E012 refusal at first parse IS the boundary for
  older runners, by the same honest-direction argument.

**Docs to update.**

- `docs/dsl.md` — the one clause at `:309` becomes the rule: which forms expand,
  from which environment, before the shell, at which three bodies, how to
  decline, and what answers a read under the decline (the body's own process
  environment, the ADR-093 overlay no longer consulted).
- `docs/workflow_authoring_pitfalls.md` — a subsection under "Shell portability
  for tool nodes": the engine answers `${NAME}` before bash does, with F1 and
  F4's mangling as the two worked examples.
- `docs/references/diagnostics.md` — a row for the lint's code (C188, or its
  successor per the allocation rule); the invalid-value refusal reuses C004's
  row, which gains the enum clause.
- Generated, never hand-edited: `task dsl:gen` rewrites
  `docs/references/dsl-properties.md`, `docs/references/dsl-grammar.md`,
  `docs/references/author-schema.md`, `SKILL.md`,
  `bots/whats-next/skills/iterion-dsl-quickref.md`, the Monaco vocabulary
  (`studio/src/lib/iterDsl.generated.ts`) and the three author JSON Schemas;
  `task map:gen` rewrites `docs/references/map-docs.md`, where this record's
  index row comes from.

## Plan review

The implementation plan carries its own slices, witnesses and mutation proofs.
No cross-family plan review was authorised for this ticket (operator decision,
2026-10-12): the plan is reviewed by Claude adversarial agents, each slice gets a
local adversarial round, and so does the whole diff before every push.

Three plan-review rounds ran on 2026-10-20 (contract; security, tenancy and
philosophy; delivery realism — 27 findings, four HIGH) and are integrated into
this record and the plan: the reader predicate now replays the engine's own
segmentation instead of probing the IR pass, the cloud twin carries the
`SchemaVersion` bump its additive-intent rule requires, the migration
paragraph names E012 and declines a floor pin, the preview report covers both
arms the engine's environment decides, the answering environment under `none`
is named, and the record's header now leads with the prose paragraph the map
generator parses. An integrate pass over the three rounds then reconciled the
two adopted readings of `ToolBodyEnv`'s second return into one contract —
`[]BracedEnvRead`, name plus `:-` arm, every read processed — which the
both-arms report and its witness require.

## A note on this record's number

Numbered 127 in parallel with the agent-node-lifetime record that merged first
and holds 127 on main; 126-129 were all taken by the time this record rebased
(2026-10-20), so it takes 130 — the next free number. The record was never
pushed under 127, so no published citation says 127 for this subject, and
`internal/docsguard`'s collisions test stays honest. A renumber before
publication is routine here, as ADR-124 records; once this file is pushed, the
number is the identifier and is never reassigned.
