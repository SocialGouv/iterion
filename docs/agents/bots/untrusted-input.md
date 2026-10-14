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

- `pkg/runtime.ToolSurfaceWriteReason` — the engine's ONE walk over a
  node's declaration and the route it may take. It answers with the arm
  that decided and is empty where neither the declaration nor any route
  the node may take lets it write, so the guard's message can never name
  a reason the verdict did not use (`ToolSurfaceCanWrite` is the same
  answer as a bool). A node is in the
  class on `full_access`; on a declared tool outside the read-only
  vocabulary (`bash`, `diagnostic_shell`, `write_file`, `file_edit`, an
  MCP or wrapper tool …); on an **omitted `tools:` list on a CLI
  backend**, where omission means the full native toolset
  (Write/Edit/Bash on claude_code; claw with a `fallbacks:` route onto a
  CLI backend counts too); and on a **declared list its route does not
  enforce** — claude_code turns the list into `--disallowedTools` over a
  closed native roster iterion does not own, so it removes the names
  iterion happens to enumerate and nothing else (measured on CLI 2.1.220,
  #1652: a node declaring `tools: [read_file]` still holds
  `EnterWorktree`), and pi, kimi, grok and opencode never receive the list
  at all. claw is the one route a list bounds outright — it resolves its
  tool definitions from the list and nothing else. On **codex** the list
  picks a sandbox mode and narrows nothing else, codex's built-in shell
  running outside the SDK's per-name callbacks, so the bound is the mode
  those names buy: `read_file`, `glob` and `grep` buy `read-only`, while
  `web_fetch` and the five advisory names (`git_diff`, `git_status`,
  `list_files`, `search_codebase`, `tree`) buy `workspace-write` and put
  the node in the class (`delegate.CodexDeclaredListIsReadOnly`, read by
  the engine's walk). A `fallbacks:` route onto a backend that does not
  bound the list puts the node in the class whatever its primary backend,
  since a pre-run reading is pessimistic over the whole chain. A route no
  name resolves (`backend: ""`, `auto`) is read like the first choice of
  the compiled-in detection order, claude_code. The backend is read at its
  authored default (`${VAR:-default}` → `default`), so the guard's verdict
  does not depend on the host running it; a `${VAR}` with no authored
  default names no route at all and is read the same pessimistic way —
  `default_backend:` answers for it only on a host where the reference
  names nothing, and any name the host does give wins, so this reading
  credits the default's bound to no node; the same spelling on a
  `fallbacks:` route reaches that cell whatever the node declares.
  **What it does not read**: the runtime's own appends. `auto_memory: on`
  grants `read_file`/`write_file`/`glob` and ultracode grants the unbounded
  `agent` subagent tool, both to a claw node whose `tools:` list is
  RESTRICTED. On codex an append adds no writer and does something else: the
  list picks a sandbox MODE, so `interaction:` putting `ask_user` on a
  non-empty list, `capabilities:` putting the board/runs MCP tools there, or
  a prompt referencing an image `attachments:` entry putting `read_image`
  there, buys the `workspace-write` mode, where codex's built-in shell acts
  on the worktree. All of them resolve outside the IR, so only shared-worktree
  admission (which holds an executor) reads them. A claw node that
  restricts `tools:` and enables `auto_memory:`, or a codex node that
  restricts them and sets `interaction:`, holds more than it declared and
  joins this class by hand.
- any capability other than `board.read` / `runs.read`: `board.create`,
  `board.comment`, `board.label`, `board.assign`, `board.move`,
  `board.close` — a comment or a label written off injected text is a
  durable write.
- `readonly: true` does **not** exempt a node: codex enforces it as a
  read-only OS sandbox and pi as a four-reader tool list, while claude_code
  only withholds the file-writing tools and runs under `bypassPermissions`
  with whatever else the tool list leaves visible — a shell included.

**Enforcement:** `bots/catalog_untrusted_input_boundary_test.go` compiles
every catalog bot, classifies every agent and judge with the rule above,
and fails on any member whose system prompt lacks the phrase. The only
exemption is a per-agent `deferred` entry naming the session that owns the
bot, and a stale entry (the paragraph landed, or the node stopped acting)
fails the test too. There is no allowlist of members still to do: #1494
drained the last 104, so every acting prompt the walk reaches carries the
paragraph today except the `deferred` ones, and a new one reddens the guard
until it does. A second test over the same walk keeps the class's MESSAGE
as true as its verdict — every member says what put it there — and holds a
floor under the member count, so a predicate that stopped classifying
cannot report the same green as one that classifies them all.

Two obligations come with it. A prompt that LEAVES the class has to be named
in the guard's `bounded` list, or the complement test reddens: a node nobody
named is read as one that quietly stopped owing the paragraph. And the class
itself is a committed artifact —
`bots/testdata/untrusted_input_boundary_members.txt`, one
`<bundle>/<node><TAB>reason` line per reason — whose diff is the review, so a
new bot, a new capability or a reworded reason makes it stale and the gate
says so. Regenerate it with `task bots:boundary:gen` and review the diff;
never edit it by hand.

The walk is every `*.bot` of every bundle — not just each
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

