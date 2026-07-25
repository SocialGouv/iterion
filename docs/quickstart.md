# 🚀 Quickstart

For developers who already live in AI coding tools. Zero to a real agent
workflow — running, watched, and landed — in a few minutes.

## 1. Install

```bash
curl -fsSL https://socialgouv.github.io/iterion/install.sh | sh
iterion version
```

No API key yet? That's fine — step 2 runs with no credentials.

## 2. Run the engine (no credentials)

Prove the pipeline works with a deterministic, LLM-free example — it compiles a
`.bot` to a graph and executes it:

```bash
iterion run examples/else_edge.bot --var n=100
```

You get a typed run: a persisted event log, versioned node artifacts, and a
final result. Inspect it:

```bash
iterion inspect --events
```

## 3. See it — and launch visually

```bash
iterion studio
```

The browser studio is the graph editor, the run console, and a Launch form in
one. Open a `.bot`, watch nodes light up live, answer human gates, and follow
cost/tokens as the run goes.

## 4. Run a real bot on your repo

Iterion **auto-detects** whatever you already have signed in — Claude Code
OAuth (the subscription "forfait"), `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and
more (see [backends](/backends)). With one available, point a catalogue bot at a
repo. For example, a read-only cross-family review of your current branch:

```bash
cd /path/to/your/repo
iterion run /path/to/iterion/bots/review-pr/main.bot
```

Or ship a whole feature autonomously (runs in an isolated git worktree, on by
default):

```bash
iterion run bots/feature-dev/main.bot \
  --var feature_prompt="Add a --json flag to the export command" \
  --max-cost-usd 5
```

Everything is bounded: `--max-cost-usd`, `--max-tokens`, `--max-duration`. A run
that hits the cap pauses resumably — raise it and `iterion resume`.

## 5. Operate the run

```bash
iterion runs list                 # every run, with status
iterion report --run-id <id>      # a chronological report
iterion resume --run-id <id> --file <bot>   # continue a paused/failed run
```

A `worktree: auto` bot lands its commits on a named branch and (best-effort)
fast-forwards your checked-out branch — the studio RunHeader always shows where
the work went. Nothing touches your tree unless a guard passes.

## 6. Make it yours

The catalogue is a starting point, not a cage:

```bash
iterion bots list                 # discover the fleet
iterion bots create my-bot        # scaffold a new bundle
```

Fork any bot, edit its `main.bot` in the [studio builder](/visual-editor), and
learn the language from the [DSL guide](/dsl). When you're ready to operate
agents for a team — orgs, quotas, webhooks, audit — see
[Iterion Cloud](/cloud-overview).

---

Stuck on credentials or backend selection? [Backends & auto-detection](/backends)
covers every path. Want the vision first? [Why Iterion?](/why-iterion)
