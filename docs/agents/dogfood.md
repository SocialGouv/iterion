# Dogfood discipline — a run the operator cannot watch does not count

Launching a catalog bot against this repo for real: where the run must land so
it is visible, how to contain its side effects, why the installed binary's
freshness decides what capabilities the agent actually gets, and the bilan that
makes the run survive the gitignored artifacts.

### Live dogfood runs MUST be visible in the operator's studio

When you test or dogfood a catalog bot with a real run, launch it into the
store the operator's running `iterion studio` reads. `iterion run` anchors its
store on the **working directory**, so from a workspace whose `.iterion` is
already a managed store (it has `runs/`, `dispatcher/` or `.iterion-store`) the
run lands in `<workspace>/.iterion` and the studio sees it.

The caveat is a workspace with no managed `.iterion` yet: the run then goes to
`~/.iterion/projects/<workdir-key>/`, which the operator's studio (bound to
`<workspace>/.iterion`) cannot see, producing a `run not found … run.json: no
such file or directory` 404 in the studio's run/diffs panel. When in doubt
**pass `--store-dir "$PWD/.iterion"` explicitly**. And **never** use a
throwaway `--store-dir /tmp/...`. A run the operator can't watch in the UI does
not count as validated.

Contain side-effects with per-run **flags**, not by hiding the run in a
separate store:
- board writes → `--var post_to_board=false` (or the bot's equivalent),
- worktree/branch changes → `--merge-into none` (commits land on a storage
  branch only, never the operator's checked-out branch),
- report/scratch output → a scratch `report_path` (e.g. under `/tmp`).
- **`worktree: auto` bots: don't pass `--var workspace_dir=$(pwd)`** — omit it
  so it defaults to `${PROJECT_DIR}`, which the engine resolves to the worktree
  (the clean, fully-mounted tree). A literal repo-root override aims agents at
  the main checkout, which under sandbox has `.git` mounted but no working-tree
  files → git there reports a phantom "all files deleted". The engine now
  auto-remaps a repo-root override back to the worktree (with a warning), but
  omitting it is cleaner.
- **Sandboxed dogfood fixtures must NOT live under `/tmp/claude-<uid>/`**
  (the Claude Code scratchpad, e.g. `/tmp/claude-1000/...`). Docker creates
  the bind target's missing parent dirs root-owned inside the container,
  which shadows the in-container Claude CLI's own temp root
  (`/tmp/claude-$UID`) — claude then hangs silently before its first stdout
  byte, so every claude_code attempt dies on the 90s cold-phase timeout
  (surgically isolated 2026-07-07 while validating native:221edac8: the
  same fixture at `/tmp/probe-fixture` boots in 3s, at
  `/tmp/claude-1000/<x>` it hangs). Clone fixtures to a neutral path
  (e.g. `/tmp/iterion-probe-<x>/`) before a sandboxed run.

The same applies to a dedicated server instance you spin up from a worktree to
exercise modified engine code: bind it to the operator's store dir (or tell
the operator the port) so the runs are observable.

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

**Keep the installed binary fresh — delegated subprocesses use it, not the
running code.** Bot capabilities that run out-of-process — the `__mcp-board`
server (board.* tools), the sandboxed `__claw-runner`, the `__mcp-ask-user`
server — are spawned via `proc.LocateIterionBinary()`. Under `task studio:dev`
(`go run`) the studio's own `os.Executable()` is a volatile build path, so
LocateIterionBinary **falls back to the installed `/usr/bin/iterion`** (then
`/usr/local/bin`, `~/.local/bin`). If that install is older than your working
tree, agents silently get the **stale** capability set — e.g. a dogfood run saw
the board MCP advertise only 7 tools (no `set_bot`/`list_labels`) because the
installed binary predated them, and the agent (correctly) fell back to routing by
`assignee`. After adding or changing any delegated capability, **reinstall the
binary** or export `ITERION_BIN=<fresh binary>` for the studio process —
otherwise the gap reads as an agent/bot bug when it's a stale binary.

**The installed binary must be built STATIC (`CGO_ENABLED=0`)** — it is
bind-mounted into sandbox containers (`addClawBinaryMount` → `/usr/local/bin/iterion`)
so the in-container `iterion __claw-runner` can run. devbox's default is
`CGO_ENABLED=1`, so a plain `devbox run -- go build` produces a binary
**dynamically linked against nix glibc**; it runs on the host but fails inside a
container with `exec: /usr/local/bin/iterion: no such file or directory` (the nix
ld-linux loader isn't there). Always refresh the install from a static build:
`CGO_ENABLED=0 devbox run -- go build -o ./iterion ./cmd/iterion && sudo cp
./iterion /usr/bin/iterion` (or `devbox run -- task build`, which already pins
`CGO_ENABLED=0`). The production sandbox images can also ship their own static
iterion on PATH, which sidesteps the host-mount entirely.

**In dev, `task studio:dev` now handles this for you** — `studio:dev:backend`
builds a static `./iterion` (`CGO_ENABLED=0`) and runs *that* (with `ITERION_BIN`
pinned to it) instead of `go run`, so every watchexec restart hands the delegated
subprocesses a fresh, static, matching binary with **no `sudo cp`**. The manual
install refresh above is only for non-dev setups (plain `iterion studio` /
`server` / `dispatch`) or a stale system install.

**Binary-freshness gotcha:** the full typed `remote` surface is recent — an
older installed binary may expose only `api/login/logout/status/openapi/routes`
(the `remote api` escape hatch still reaches everything). If subcommands are
missing, refresh the install from a static build (see the binary-freshness note
above). Smoke-test claude_code auth on a cloud runner (e.g. a
Claude-subscription **forfait** via `CLAUDE_CODE_OAUTH_TOKEN`) with a one-node
`backend: "claude_code"` bot: `system/init … model=claude-opus-5` in the run
log + `0 tokens` billed confirms the OAuth-forfait path (not a metered API key).

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
[docs/workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md), not
the per-bot file. The bilan is **one of three knowledge channels — keep them
distinct**: workspace memory (`~/.iterion/projects/.../memory/`, per-operator,
gitignored — [docs/memory-and-knowledge.md](../memory-and-knowledge.md)) is
session scratch; **board issues** are open tasks; **bilans** are the durable,
committed, PR-reviewable record. Index + template:
[docs/bot-runs/README.md](../bot-runs/README.md).

