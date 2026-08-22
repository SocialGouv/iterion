# Copi — `bots/copilot`

Conversational iterion assistant: the DSL, the Cxxx diagnostics, run/resume
semantics, backends. Read-only by construction. Newest run first.

---

## 2026-08-22 — first dogfood, with cross-review on (runs `01a02a31`, `01a02a32`, `01a02a39`)

- **Status**: validated — after three attempts, two of which failed on real defects the bot's tests could not have caught.
- **Versions**: bot 0.1.0 · iterion `1ae6b850` (branch `feat/assistant-epic`)
- **Method**: `iterion run bots/copilot/main.bot --store-dir "$PWD/.iterion" --var reviewer=on --var initial_message=…`. Copi on `claw` + `openai/gpt-5.6-sol`; reviewer on `claude_code` + `claude-fable-5`. No board writes, no worktree (`worktree: none`), `sandbox: none`.
- **Result**: converged to the chat pause on every successful turn. 3 answering turns + 3 review turns. **$2.46 – $2.57 per reviewed turn**, essentially all of it the reviewer.

### Value — the reviewer earns its place, but not on every turn

Turn 1 ("explique C176") returned an **empty critique**. That is the designed
verdict for a sound answer, and it was correct: Copi's answer was accurate,
with `file:line` citations that matched what a human reading the same code
found independently.

Turn 2 was a leading question — *"je vais mettre `sandbox: none` et
`permission: off` sur tous mes bots de chat, c'est bien ça ?"*. The reviewer
bit, and usefully: it caught Copi **inverting the semantics of
`permission: deny`**, citing `docs/permissions.md:38`. Third run, same
question shape: the reviewer confirmed the answer and then added the caveat
Copi had left out — that a `permission:` boundary is only real on
claude_code, claw and pi, quoting `docs/permissions.md:157-159`.

So: silence when there is nothing to say, a specific and sourced objection
when there is. That is the behaviour the prompt asks for, and it held.

**The economics are the open question.** $2.46 to return an empty critique is
a real number, and on a standing conversation it compounds. Off-by-default is
right; whether an operator would ever leave it on for a long session is not
established by three turns.

### Findings — three defects, all found by running, none catchable by the tests

1. **`tools: [list_files]` — unknown tool, run dies on the first node.**
   `list_files` is not a registered claw tool; the registry rejects it at
   execution. `iterion validate` compiled it happily: **it does not check tool
   names against the registry**. The canonical builtins are
   `read_file, write_file, glob, grep, file_edit, web_fetch, bash`
   (`pkg/backend/tool/claw_builtins.go:91-98`), plus `skill` with a `skills:`
   block. A `Cxxx` diagnostic for unknown tool names on claw nodes would have
   turned a failed run into a compile error — worth a ticket.

2. **`concat()` is the ARRAY primitive; string joining is `+`.**
   `compose` and `gate` both used `concat(a, b, c)` on strings. It compiled,
   and **turn 1 passed** — `if()` short-circuits, and that turn's critique was
   empty, so the faulty branch never evaluated. The failure surfaced only on
   the turn where the reviewer had something to say, i.e. the first turn where
   the feature did its job. Pinned by
   `TestCopilot_CrossReview_ComposesBothHalves`, which drives the branch with a
   non-empty critique.

3. **`{{input.x}}` on an EDGE resolves against the RUN's inputs, not the source
   node's input — while the compile-time check validates against the source
   node's schema.** C034 rejects `{{input.reviewer}}` on `chat -> copi` because
   `chat_input` has no such field; at run time the same reference on
   `copi -> gate` reads the run inputs. The bot worked by coincidence, because
   `--var reviewer=on` populates both. Replaced with `{{vars.reviewer}}`, which
   says what is meant. **The compiler and the runtime disagreeing about what
   `input` means is an engine issue, not a bot one** — worth a ticket.

### Engine hardening

- Nothing committed to `pkg/` from this run. Two candidate tickets above
  (unknown-tool diagnostic; `{{input.x}}` edge-vs-compile divergence).
- Related and already filed: **#476** — grok and kimi cannot enforce
  `permission:`, which is why Copi's model ladder runs on claw rather than the
  CLI forfaits an operator would prefer.

### Lessons for next run

- **The reviewer must not share the answering model's family, and on this host
  that is not free.** `anthropic/…` on claw is *not usable* when the only
  Anthropic credential is the Claude Code OAuth forfait (`iterion models`
  reports every Anthropic row usable via claude_code alone). The reviewer's
  primary would have failed every turn and fallen through to the openai rung —
  Copi's own family — turning cross-review into a mirror, silently. It runs on
  `backend: "claude_code"` for that reason, which is legal because claude_code
  enforces the gate and the node is `session: fresh`.
- **A leading question is the test that matters.** "Explain X" produced an
  empty critique three times; "I'm going to do X, right?" is what made the
  reviewer speak. Any future evaluation of this feature should use the second
  shape.
- Measure the reviewer's cost against a real session before recommending it to
  anyone. Three turns is not a sample.
- **Do not put the run store in `/tmp` on this host** — it is a 16 GB tmpfs.
  An earlier studio in this same session filled it and took the machine's
  shell with it. See `HANDOFF-worktree-pool.md`.
