# ADR-061: Per-backend system-prompt composition mode

- **Status**: Accepted
- **Date**: 2026-07-07
- **Authors**: Adry
- **Code**: [pkg/backend/delegate/delegate.go](../../pkg/backend/delegate/delegate.go) (`SystemPromptMode`, `SystemPromptModeForBackend`, `agenticOperatingPosture`, `Task.BuildSystemPrompt`)

## Context

A node's `system:` prompt in the `.bot` DSL is the **task**, never the
whole operating posture — the read-before-edit / plan-then-act /
evidence-over-guess / TodoWrite instincts that make an agent adaptive.
Where those instincts come from differs sharply by backend:

- **claude_code** shells out to the `claude` CLI, which ships a rich
  native system prompt carrying exactly that posture.
- **claw** is `claw-code-go`, a bare API client with **no** native
  system prompt of its own — whatever iterion supplies is the entire
  prompt the model sees.
- **codex** takes the author text as the whole prompt.

A single uniform policy breaks one backend or the other. Replacing the
system prompt on all backends via `--system-prompt` strips Claude
Code's native posture and made iterion-via-Claude-Code measurably
dumber — the exact parity regression this seam exists to prevent.
Prepending an authored posture on all backends double-stacks it against
Claude Code's native one. The composition rule has to be **per-backend**.

## Decision

`delegate.SystemPromptMode` is a three-valued enum selected per backend
by [`SystemPromptModeForBackend`](../../pkg/backend/delegate/delegate.go):

- **claude_code → `SystemPromptAppendToNative`**: `BuildSystemPrompt`
  emits only the author text plus suffixes (interaction / ultracode /
  secrets / calibration / skills / preset). The claude_code backend
  routes that result to the CLI's `--append-system-prompt`, so Claude
  Code's native agentic prompt stays the base and the author text is
  additive.
- **claw → `SystemPromptAuthoredBase`**: `BuildSystemPrompt` prepends
  the iterion-authored `agenticOperatingPosture` — a short,
  provider-neutral parity substrate (tool-use discipline, plan-then-act,
  evidence-over-guessing, converge-and-stop) — before the author text,
  because claw has no native prompt to append to.
- **codex / fallback / unset → `SystemPromptStandalone`** (the zero
  value): the author text is the whole prompt, preserving pre-existing
  behaviour for any Task that never sets the mode.

The mode lives on `Task.SystemPromptMode`; the executor sets it from the
resolved backend name, so a single prompt-assembly path
([`Task.BuildSystemPrompt`](../../pkg/backend/delegate/delegate.go))
serves backends with very different baselines. The `converge-and-stop`
clause in `agenticOperatingPosture` is written to reinforce, never gate,
the loop bots' asymptote machinery.

## Trade-offs

| Dimension | AppendToNative (claude_code) | AuthoredBase (claw) | Standalone (codex) |
|---|---|---|---|
| Base posture source | Claude Code native prompt | iterion `agenticOperatingPosture` | none (author text only) |
| Parity risk | native prompt drifts out of our control | we must maintain the substrate | agent is un-postured |
| Prompt-cache stability | native base is CLI-owned | authored base is byte-stable | trivially stable |

The honest concession: iterion now maintains **two** definitions of the
agentic posture — Claude Code's (which it does not control and which can
drift between CLI versions) and its own `agenticOperatingPosture` for
claw. Parity between the two is a manual, ongoing obligation rather than
a guaranteed invariant.

## Alternatives considered

### 1. Replace the system prompt on all backends (`--system-prompt`)

Use one authored prompt everywhere and pass it to claude_code via
`--system-prompt` (replace) rather than `--append-system-prompt`.

**Rejected because**: it strips Claude Code's native posture
(TodoWrite / plan-before-act / read-before-edit / refusal posture),
which measurably degraded iterion-via-Claude-Code below native quality.
The native prompt is an asset to build on, not overwrite.

### 2. Prepend the authored posture on all backends uniformly

Always front the author text with `agenticOperatingPosture`, including
claude_code.

**Rejected because**: on claude_code the posture is already present
natively, so this double-stacks it — wasted context tokens and
conflicting/duplicated instructions, with no parity benefit.

## Consequences

- **Backend adaptivity parity without touching convergence.** claw
  behaves as adaptively as claude_code because it now carries an
  equivalent operating posture; the machinery that drives loop-bot
  convergence is untouched.
- **The composition choice is centralised.** `SystemPromptModeForBackend`
  is the single place the append/prepend/standalone decision is made, so
  the executor and any other Task constructor stay consistent.
- **The zero value is safe.** `SystemPromptStandalone` is `iota`-zero, so
  any Task that never sets the mode (tests, legacy callers) keeps the old
  author-text-is-everything behaviour.
- **`agenticOperatingPosture` is a maintenance surface.** It must stay
  short, provider-neutral (claw drives anthropic + openai), and roughly
  aligned with Claude Code's evolving native posture — a standing manual
  obligation, not an enforced invariant.
- **Re-challenge**: if `claw-code-go` grows its own native system prompt,
  `AuthoredBase` becomes redundant; if Claude Code exposes an explicit
  compose-mode flag, the append/replace choice moves there.
