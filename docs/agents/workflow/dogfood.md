# Dogfood discipline — a run the operator cannot watch does not count

Launching a catalog bot against this repo for real: where the run must land so
it is visible, how to contain its side effects, why the installed binary's
freshness decides what capabilities the agent actually gets, and the bilan that
makes the run survive the gitignored artifacts.

**Dogfood first, when it fits.** Before implementing by hand, ask whether a
catalog bot can do the work and propose launching it — regularly, never
imposed. A run is visible in the operator's studio, monitored, closed by a
bilan; every friction it surfaces improves the bot.

### Live dogfood runs MUST be visible in the operator's studio

When you test or dogfood a catalog bot with a real run, launch it into the
store the operator's running `iterion studio` reads: `iterion run` anchors its
store on the **working directory**, so from a workspace whose `.iterion` is a
managed store (`runs/`, `dispatcher/` or `.iterion-store`) the run lands in
`<workspace>/.iterion` and the studio sees it.

The caveat is a workspace with no managed `.iterion` yet: the run then goes to
`~/.iterion/projects/<workdir-key>/`, which the operator's studio (bound to
`<workspace>/.iterion`) cannot see, producing a `run not found … run.json: no
such file or directory` 404 in the studio's run/diffs panel. When in doubt
**pass the primary checkout's store explicitly** — from a linked worktree too,
since sessions work in one ([worktrees.md](worktrees.md)):
`--store-dir "$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")/.iterion"`.
And **never** a throwaway `--store-dir /tmp/...`: a run the operator can't
watch does not count as validated.

Contain side-effects with per-run **flags**, not by hiding the run in a
separate store:
- board writes → `--var post_to_board=false` (or equivalent),
- worktree/branch changes → `--merge-into none` (commits land on a storage
  branch only, never the operator's checked-out branch),
- report/scratch output → a scratch `report_path`.
- **`worktree: auto` bots: don't pass `--var workspace_dir=$(pwd)`** — omit it
  so it defaults to `${PROJECT_DIR}`, which the engine resolves to the worktree
  (the clean, fully-mounted tree). A literal repo-root override aims agents at
  the main checkout, which under sandbox has `.git` mounted but no working-tree
  files → git there reports a phantom "all files deleted". The engine now
  auto-remaps a repo-root override back to the worktree (with a warning), but
  omitting it is cleaner.
- **Sandboxed dogfood fixtures must NOT live under `/tmp/claude-<uid>/`**
  (the Claude Code scratchpad): Docker creates the bind target's missing
  parents root-owned, shadowing the container's own `/tmp/claude-$UID`, and
  claude hangs before its first stdout byte until the 90s cold-phase timeout
  (measured 2026-07-07: same fixture boots in 3s at a neutral path). Clone
  fixtures to a neutral path first.

A dedicated server instance spun up from a worktree binds to the operator's
store dir (or announces its port), so the runs stay observable.

**Do NOT dogfood a code-editing bot on the live tree under `task studio:dev`.**
The dev backend runs under `watchexec -r -e go -w cmd -w pkg -w vendor`. Because
of the `-e go` filter, only a **`.go` edit under `cmd/`/`pkg/`/`vendor/`** trips it
(a docs bot writing `.md`, or a studio bot writing `.ts`, is unaffected). So the
moment a code-mutating bot (Willy/Featurly/Billy/Renovacy/Devy) edits a watched
`.go` file on the live tree, watchexec restarts the backend and **drains the
in-flight run** (`"server drained: studio process
shutting down"` → `failed_resumable`). Bots with `worktree: auto` are mostly
insulated (their edits land in `.iterion/worktrees/<run-id>`, outside the watched
paths) — but **Willy (`whole-improve-loop`) edits the live workspace directly**
(no `worktree: auto`) and will cancel its own run this way. To dogfood a
live-tree-editing bot: launch it via a CLI `iterion run` (a separate process
watchexec's restart can't cancel) or against a non-watchexec studio
(`iterion studio` from the built binary), not the `task studio:dev` backend.

**Keep the installed binary fresh — delegated subprocesses AND a bot's own
shell tools use it, not the running code.** Bot capabilities that run
out-of-process — the `__mcp-board` server (board.* tools), the sandboxed
`__claw-runner`, the `__mcp-ask-user` server — are spawned via
`proc.LocateIterionBinary()`. **The same resolver also feeds the run's PATH**
on host runs (`--sandbox none` and every cloud run whose runner pod is the
isolation boundary): a per-run shim directory holding one `iterion`
symlink to the engine binary is prepended to PATH — never the binary's
whole directory, which also holds node/go/git/devbox the bot's devbox
pins — via [pkg/runtime/devbox_host.go](../../../pkg/runtime/devbox_host.go)'s
`provisionHostDevbox`, so a bot's shell tools (a `tool` node's `command:`,
claw's `diagnostic_shell`, a `claude_code` Bash call) resolve `iterion` to
THIS engine — not to whatever `iterion` sits earlier on the operator's
ambient PATH. Sandboxed runs bind-mount `/usr/local/bin/iterion` into the
image, so the container's own iterion serves there instead. Under `task
studio:dev` (`go run`) the studio's own `os.Executable()` is a volatile build
path, so LocateIterionBinary **falls back to the installed
`/usr/bin/iterion`** (then `/usr/local/bin`, `~/.local/bin`). If that install
is older than your working tree, agents silently get the **stale** capability
set — e.g. a dogfood run saw the board MCP advertise only 7 tools (no
`set_bot`/`list_labels`) because the installed binary predated them, and the
agent (correctly) fell back to routing by `assignee`. A separate wave-4
dogfood of copilot spent twenty minutes hunting a C145 warning that its
shell's `iterion validate` did not print, because `~/.local/bin/iterion` was
v3.112.2 while the engine was the branch build (#1384; now closed at the
chokepoint above). After adding or changing any delegated capability, OR
after building a fresh engine you want a shelling bot to see, **reinstall the
binary** or export `ITERION_BIN=<fresh binary>` for the studio process —
otherwise the gap reads as an agent/bot bug when it's a stale binary.

**The installed binary must be built STATIC (`CGO_ENABLED=0`)** — it is
bind-mounted into sandbox containers, and devbox's default build is dynamically
linked against nix glibc: it runs on the host but dies in-container with
`exec: /usr/local/bin/iterion: no such file or directory`. `task build` pins
`CGO_ENABLED=0`; refresh the install from a static build. In dev,
`task studio:dev` builds and pins a fresh static binary for you — the manual
refresh is only for non-dev setups or a stale system install.

**Forfait smoke test:** a one-node `backend: "claude_code"` bot on a
Claude-subscription forfait shows `system/init … model=claude-opus-5` in the
run log with `0 tokens` billed — that confirms the OAuth-forfait path, not a
metered API key.

### This repo's own `.mcp.json` does not reach a sandboxed claw node

`.mcp.json` at the root of this repository declares two servers, `engine` and
`sentry`. They are `project`-origin, so the launcher of a **sandboxed** run
does not start them — and every dogfood run here is sandboxed by default: the
devcontainer plus `--sandbox auto` is the normal shape. A claw node therefore
builds its task WITHOUT their tools and the run record carries one
`mcp_server_degraded` event per server (`source: ambient`, `origin: project`,
`refused: true`) — nothing is broken, and a bot whose judgement depends on
those tools is quietly weaker than the same bot on the operator's host.

Three ways out, in order of preference: route the node to `claude_code` or
pi, which start the servers inside the container; declare the node's
`fallbacks:` so the typed refusal walks to such a route; or run that bot with
`--sandbox none` and say so in the bilan. A node that NAMES one of their
tools (`tools: [mcp.engine.…]`) is refused at execution rather than degraded,
so the run fails loudly instead of thinking less — prefer that over hoping.
Origins and the full table: [sandbox.md § MCP servers under a
sandbox](../../sandbox.md#mcp-servers-under-a-sandbox).

### Every dogfood run gets a bilan in `docs/bot-runs/<bot>.md`

The run artifacts under `.iterion/runs/<id>/` are gitignored — they vanish from
everyone but you. So when you dogfood a catalog bot, **the run does not count as
done until you've written a dated bilan** to `docs/bot-runs/<bot>.md` (named by
bot **directory**, e.g. `whole-improve-loop.md`, not by persona). This is the
repo's committed bot knowledge base: the next contributor reads a bot's file
before launching it — what it caught, what it missed, what to change, which
engine bugs the run surfaced. Append newest-first, one section per run:

```markdown
## YYYY-MM-DD — <short label> (run <id-prefix>)
- Status: validated | partial | failed
- Versions: bot <manifest version> · iterion <git sha>
- Method: backend(s)/model(s), budget, key --vars, flags (--merge-into, post_to_board, sandbox image)
- Result: converged? iterations, cost $, duration, where commits landed (branch/sha)
- Value: the high-value thing it actually produced (or: low value + why)
- Findings / misses: what the bot caught or missed
- Engine hardening: iterion bugs found → commits/ADRs
- Lessons for next run: what to change (vars, prompt, scanner, skill)
```

Cite the run-id; the full chronological report is reconstructable any time with
`iterion report --run-id <id> --output /tmp/<bot>-<id>.md`. Cross-bot lessons
(Goodhart, façade, asymptote) still go in
[docs/workflow_authoring_pitfalls.md](../../workflow_authoring_pitfalls.md), not
the per-bot file. The bilan is **one of three knowledge channels — keep them
distinct**: workspace memory (`~/.iterion/projects/.../memory/`, per-operator,
gitignored — [docs/memory-and-knowledge.md](../../memory-and-knowledge.md)) is
session scratch; **board issues** are open tasks; **bilans** are the durable,
committed, PR-reviewable record. Index + template:
[docs/bot-runs/README.md](../../bot-runs/README.md).

