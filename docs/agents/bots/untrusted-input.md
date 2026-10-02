# The UNTRUSTED INPUT BOUNDARY

Read it when a node's prompt can carry repo- or issue-derived text and the node acts: shells out, writes, files issues.

## Prompts that can act carry the UNTRUSTED INPUT BOUNDARY

Every LLM node of a catalog bot reads material it did not write — the
target repository's source, tests, docs and commit messages, the tickets,
reviews and PR text handed to it, scanner output, tool logs. That material
may say WHAT the work is; it must never change HOW the node operates. A
node that can also ACT on it — write files, shell out, file or comment on
board issues — must be told so in its system prompt, with a paragraph
headed `IMPORTANT — UNTRUSTED INPUT BOUNDARY:` that names what is data,
what is an authoritative instruction (the system prompt, the operator's
mission as the workflow delivers it, the skills the node loads
explicitly), and what the node does with a directive it finds in the
material (report it, never obey it). The phrase is a posture cue the LLM
adopts, not a list of forbidden spellings: a guard that enumerated
directives would be widened by the next payload and never converge.

**Who is in the class.** Membership is decided by the engine's own
notion of what the node can do, never by grepping tool names:

- `pkg/runtime.ToolSurfaceCanWrite` — `full_access`, a declared tool
  outside the read-only vocabulary (`bash`, `diagnostic_shell`,
  `write_file`, `file_edit`, an MCP or wrapper tool …), or an **omitted
  `tools:` list on a CLI backend**, where omission means the full native
  toolset (Write/Edit/Bash on claude_code; claw with a `fallbacks:` route
  onto a CLI backend counts too). The backend is read at its authored
  default (`${VAR:-default}` → `default`), so the guard's verdict does not
  depend on the host running it.
- any capability other than `board.read` / `runs.read`: `board.create`,
  `board.comment`, `board.label`, `board.assign`, `board.move`,
  `board.close` — a comment or a label written off injected text is a
  durable write.
- `readonly: true` does **not** exempt a node: the codex and pi delegates
  enforce it as a sandbox mode, claude_code never reads it and runs under
  `bypassPermissions` with whatever the tool list leaves visible.

**Enforcement:** `bots/catalog_untrusted_input_boundary_test.go` compiles
every catalog bot, classifies every agent and judge with the rule above,
and fails on any member whose system prompt lacks the phrase. The only
exemption is a per-agent `deferred` entry naming the session that owns the
bot, and a stale entry (the paragraph landed, or the node stopped acting)
fails the test too. There is no allowlist of members still to do: #1494
drained the last 104, so every acting prompt the walk reaches carries the
paragraph today except the `deferred` ones, and a new one reddens the guard
until it does. The walk is every `*.bot` of every bundle — not just each
`main.bot`, because a bundle may ship sibling entrypoints (golden-master) or
none at all (smoke) — plus the dispatcher's zero-config fallback
(`pkg/cli/templates/dispatch_bots_default.bot`), which is compiled into every
binary and runs against a raw issue body.

A second guard keeps the paragraph a LITERAL region: no template reference of
any namespace between the marker and the blank line that closes it. Naming a
field with `{{input.x}}` instead of `` `x` `` renders the VALUE there, and a
system prompt goes through the same resolver as a user prompt — so the
paragraph would deliver the untrusted text into the authoritative half of the
prompt it exists to protect. Write the field NAME; put anything that must
render outside the paragraph.

When you add a node that writes, files, patches or shells out, write the
paragraph in the same change — the reference wordings are
`report_card_system` and `triage_system` in
`bots/sec-audit-source/main.bot`, `campaign_system` in
`bots/feature-dev/lib/prompts.bot` for an implementer, and
`verify_system` there (the prompt of the `verify_build` node) for a node
that runs the repository's own commands.

