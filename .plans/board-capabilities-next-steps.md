# Board capabilities — next steps (handoff to a sandboxed agent)

## Context

The `iterion/board-caps` branch (worktree `.works/board-capabilities`,
3 + 1 commits ahead of `main`) shipped the full plumbing for bots to
write to the native kanban board via capability-gated MCP tools. The
12-step plan is complete and all unit + e2e suites are green.

What this hand-off needs from a host-side agent that **can spawn real
sandboxed `iterion run` workflows** and **can read OAuth/API keys**:

1. **Smoke-test the live stdio path** end-to-end with a real Claude
   Code subprocess (not just the e2e stub).
2. **Wire the sandbox HTTP path** from the runtime to the editor
   server's token registry — currently `Task.BoardHTTPEndpoint` /
   `Task.BoardRunToken` are typed but never populated, so a
   `sandbox: auto` workflow with `capabilities: [board.*]` logs a
   warning and disables the board.
3. **Round-trip the `whats-next.bot` PO mode**: real run, real
   issues land on the board, conductor picks them up, dispatches
   the assigned bots, and they complete.
4. **Regenerate the bot catalog skill** so the heuristics surface
   the new bot fleet rather than the stale text.

Each task below is self-contained; pick them up in order or in
parallel as fits the environment.

---

## Task A — Live stdio smoke test (non-sandbox)

**Goal.** Confirm that a real `claude_code` subprocess sees the
`mcp__iterion_board__*` tools advertised by `__mcp-board` and can
create + transition issues.

**Pre-reqs.** Either `ANTHROPIC_API_KEY` or Claude Code OAuth on
the host. `devbox` shell active.

**Steps.**

1. Build a clean binary in the worktree:
   ```
   cd /workspaces/iterion/.works/board-capabilities
   devbox run -- task build
   ```
2. Create a minimal `.bot` file that exercises a single agent with
   `capabilities: [board.read, board.create, board.move]` — for
   example reuse [examples/whats-next/main.bot](examples/whats-next/main.bot)
   stopping at `emit_action`, or write a 20-line `bots/smoke/board_smoke.bot`
   with a one-shot agent that calls `mcp__iterion_board__create_issue`
   twice and `mcp__iterion_board__transition_issue` once.
3. Run with an isolated store dir so the host's existing board is
   not touched:
   ```
   ./iterion run path/to/board_smoke.bot \
     --store-dir /tmp/iterion-board-smoke
   ```
4. Inspect the result:
   ```
   ./iterion issue list  --store-dir /tmp/iterion-board-smoke
   ./iterion issue board --store-dir /tmp/iterion-board-smoke
   ```
   Expectation: two issues exist, one in the post-transition state.
5. Tail the run log for `mcp__iterion_board__` tool calls:
   ```
   tail -f /tmp/iterion-board-smoke/runs/<run-id>/run.log
   ```

**Failure modes to watch for.**
- `__mcp-board` subcommand not found → the SDK couldn't resolve
  the host binary path. Look for the warning in run.log around
  the `Native ask_user MCP server` line and confirm we picked
  up the new board block in [pkg/backend/delegate/claude_code.go](pkg/backend/delegate/claude_code.go)
  (`HasBoardCapability` branch).
- `tools/list` returns zero board tools → `ITERION_BOARD_CAPS`
  env wasn't forwarded; check [Task.Capabilities](pkg/backend/delegate/delegate.go)
  is non-empty in the launched task (search the run.log for
  `Capabilities:`).
- Capability denied on a tool the bot expected → mismatch between
  the granted cap and the tool's cap; cross-reference
  [boardops.allTools](pkg/conductor/native/boardops/ops.go).

**Acceptance.** Two issues on disk, one transitioned. Time budget:
~10 min.

---

## Task B — Wire the sandbox HTTP path in the runtime

**Goal.** Make `sandbox: auto` workflows with `capabilities: [board.*]`
actually work. Today the wiring on the backend side is complete
([pkg/backend/delegate/claude_code.go](pkg/backend/delegate/claude_code.go),
[pkg/server/mcp_board_handler.go](pkg/server/mcp_board_handler.go))
but **no code populates `Task.BoardHTTPEndpoint` / `Task.BoardRunToken`**,
so the warning path fires.

**What needs to be true at run-start.**
1. The runtime knows the iterion editor server's bind address
   (`<bind>:<port>`).
2. The runtime knows the sandbox-side hostname that reaches the
   host's loopback (Docker: `host.docker.internal`; k8s: the
   `ITERION_POD_IP` injected via downward API — see
   [pkg/sandbox/iface.go::ProxyConfigurer](pkg/sandbox/iface.go)).
3. The runtime generates an opaque token (uuid is fine), calls
   `server.BoardMCPTokens().Register(token, caps)`, and stashes
   it on the per-node task.
4. On `run_finished` / `run_failed`, the runtime calls
   `Revoke(token)` — leaks otherwise (the registry has no TTL by
   design; see the godoc on `BoardMCPTokenRegistry`).

**Suggested wiring path.**

- New executor option `model.WithBoardMCPIssuer(BoardMCPIssuer)` in
  [pkg/backend/model/executor.go](pkg/backend/model/executor.go),
  where the interface is:
  ```go
  type BoardMCPIssuer interface {
      Issue(caps []string) (endpoint, token string, revoke func())
  }
  ```
- Implement the issuer in `pkg/server/`: an adapter around
  `Server.BoardMCPTokens()` + the bound `Server.Addr()`. Compute
  `endpoint` from the sandbox driver's `advertiseHost`
  (see `pkg/sandbox/{docker,kubernetes}/driver.go`).
- Plug the issuer into the executor when the engine starts a run
  with sandbox active and a node has any `board.*` cap.
- On `Execute`, call `Issue(caps)`; thread the returned endpoint /
  token onto the `delegate.Task`; on `defer`, call `revoke()`.
- Acceptance test: extend [e2e/board_conductor_test.go](e2e/board_conductor_test.go)
  with a variant that runs the agent through a fake sandbox driver
  that records the spawned MCP transport — verify it's `MCPHTTPServer`
  with the right headers.

**Sandbox network policy.** The CONNECT proxy already allows the
host loopback via `host.docker.internal` (Docker) and the pod IP
(k8s). Confirm with `iterion sandbox doctor` after the wiring.
If the policy preset rejects the host URL, extend the default
allowlist in [pkg/sandbox/netproxy/](pkg/sandbox/netproxy/).

**Acceptance.** A workflow with both `sandbox: auto` and
`capabilities: [board.create]` runs end-to-end and the bot inside
the container successfully creates an issue. ~1-2h work.

---

## Task C — `whats-next.bot` full round-trip

**Goal.** Run `examples/whats-next/main.bot` against a real repo,
let `emit_action` create + assign + move issues on the board, let
a `conduct` daemon pick them up, dispatch the assigned bots
(`vibe_feature_dev`, `branch_improve_loop`, …), and verify they
complete.

**Pre-reqs.** Anthropic key (claude_code), OpenAI key (claw for
the explore / propose / revise nodes), `conduct.yaml` aimed at the
native tracker.

**Steps.**

1. Create a conductor config:
   ```yaml
   # /tmp/whats-next-conduct.yaml
   tracker: { kind: native }
   polling:  { interval_ms: 500 }
   workspace:
     root: /tmp/whats-next-workspaces
   agent:
     workflow: /path/to/repo/examples/bots/vibe_feature_dev.bot
     max_concurrent: 1
   ```
2. Start the conductor in tmux/screen:
   ```
   ./iterion conduct /tmp/whats-next-conduct.yaml \
     --store-dir /tmp/whats-next-store --port 4892
   ```
3. In another shell, run whats-next on the target repo:
   ```
   ./iterion run examples/whats-next/main.bot \
     --var workspace_dir=/path/to/repo \
     --store-dir /tmp/whats-next-store
   ```
4. When the human_review pause hits, type free-text feedback,
   approve. Watch:
   ```
   tail -f /tmp/whats-next-store/conductor/events.jsonl
   ./iterion issue list --store-dir /tmp/whats-next-store
   ```
   The conductor should claim each `ready` issue, dispatch the
   assigned bot, and (eventually) release the claim.
5. Verify final board state: every issue in a terminal state,
   no orphaned claims, `events.jsonl` carries the full trail.

**Known gaps to flag back.**
- The conductor today dispatches a **single** workflow path per
  config. `whats-next.bot` writes `assignee: <bot-name>` per
  issue — there's no router that maps assignee → workflow yet.
  Workaround: run a conductor per assignee (one config per bot),
  filtered via `--filter assignee:<name>` (add this flag if it
  doesn't exist), OR write a tiny `conduct-multiplex` script.
  This is the **"Iterion feature gap"** the whats-next prompt
  references; record observations and file an issue if real.
- whats-next' `fields.bot_args` is a CSV string that the conductor
  router (when it exists) is supposed to split on commas. Confirm
  the format the conductor's `RoutingRunner` (if any) expects;
  align if needed.

**Acceptance.** At least one assigned bot completes end-to-end on
real issues. Write the findings (assignee routing gap, any prompt
weakness, latency) back into a `.plans/whats-next-live-2026-05-XX.md`.

---

## Task D — Regenerate `iterion-bot-catalog.md`

Once Task A passes, run:

```
./iterion bots list --format=skill --paths examples/ \
  > examples/whats-next/skills/iterion-bot-catalog.md
```

Inspect the diff — the current hand-edited skill carries assignment
heuristics that the generated decision-tree may not preserve. Either:
- Keep the generated table and append the heuristics paragraph by
  hand, or
- Add an `--include-heuristics-from <path>` flag to `iterion bots list`
  to splice an external heuristics block into the generated output.

**Acceptance.** The skill mentions every `.bot` under `examples/`
(vibe_feature_dev, branch_improve_loop, whole_improve_loop,
secured-renovacy, whats-next, doc-align if present).

---

## Task E — Open follow-ups deferred during /simplify

Low-priority cleanups the reviewer flagged but I left out of the
simplify commit:

- **Dispatch unification**: [cmd/iterion/mcp_board.go::dispatchMCPBoard](cmd/iterion/mcp_board.go)
  and [pkg/server/mcp_board_handler.go::dispatchHTTP](pkg/server/mcp_board_handler.go)
  share their switch logic. The blocker is that the JSON-RPC envelope
  types live in `cmd/iterion` (package main). Move
  `mcpRequest`/`mcpResponse`/`mcpError` to a small `pkg/internal/mcprpc/`
  package, then both transports call into a single
  `boardops.DispatchMCP` helper.
- **State-name constants in the skill**: [examples/whats-next/skills/iterion-board.md](examples/whats-next/skills/iterion-board.md)
  still hardcodes `ready` / `done` in prose. The skill is a Markdown
  doc, so the rename-safety argument doesn't apply, but if a future
  refactor renames `StateReady` it'd be nice to grep the .md too.
- **`os.Executable()` caching in `claude_code.go`**: resolved on every
  Execute call. Cache once on the backend struct. The same pattern
  applies to the `ask_user` block above the board block — refactor
  both at once.
- **Truncated `.bot` reads** in `pkg/cli/bots.go::parseBotFile`:
  for huge bot files, read only the first ~50 lines via `bufio.Scanner`
  rather than `os.ReadFile`. Not a real issue today.

---

## Handoff checklist for the host agent

- [ ] Build the binary in the worktree.
- [ ] Task A — live stdio smoke test (10 min).
- [ ] Task C — whats-next full round-trip (uses Task A's binary).
- [ ] Task B — sandbox HTTP wiring (1-2h, ships separately).
- [ ] Task D — regenerate the bot catalog skill.
- [ ] Optionally Task E (cleanups).
- [ ] Push `iterion/board-caps` to origin once Task A passes.
- [ ] PR title: `feat: bot capabilities + board MCP (stdio + HTTP) + whats-next PO mode`.
- [ ] PR body: copy the plan summary from
  `/home/jo/.claude/plans/feature-request-api-ou-jaunty-forest.md`.
