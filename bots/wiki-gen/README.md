# Wikky (`wiki-gen`)

Generate and incrementally maintain a **navigable, Open-Knowledge-Format
wiki** for any repository. One adaptive `claude_code` agent surveys the
code, plans the concept pages and their relationships, and writes a
structured wiki tree under `wiki/` — every claim grounded in the source.
Deterministic tools then regenerate the directory indexes, validate the
OKF frontmatter + intra-wiki links, and enforce a docs-only scope, so the
agent cannot ship a hallucinated or structurally-broken wiki.

## Wikky vs Doki (docs-refresh)

| | **Wikky** (`wiki-gen`) | **Doki** (`docs-refresh`) |
|---|---|---|
| Verb | **generate + maintain** a wiki | **align** existing docs to code |
| Artifact | a parallel navigable `wiki/` tree it owns | edits README / `docs/**` / CLAUDE.md in place |
| Direction | code → fresh structured wiki | docs follow code (surgical fixes) |
| Gate | valid OKF frontmatter + resolvable links + docs-only scope | mechanical anchor-drift coverage |

Reach for Doki to fix a repo's **existing hand-authored docs**; reach for
Wikky to build/keep a **navigable wiki artifact** from scratch.

## Output shape (OKF v0.1)

```
wiki/
  index.md            # generated (root; carries okf_version: "0.1")
  quickstart.md       # entrypoint — links every major concept
  architecture/
    index.md          # generated (nested; no frontmatter)
    overview.md       # concept page (type-required frontmatter)
  workflows/
  domain/
  ...
```

Each concept page opens with YAML frontmatter (`type` required; `title` /
`description` / `tags` recommended). Markdown links between pages **are**
the concept-relationship edges — so the tree is directly ingestible by a
knowledge-graph explorer (e.g. `graphify`). See
[skills/okf-format.md](skills/okf-format.md).

## How it works

```
scan_repo ──noop?──▶ done              (wiki already current for HEAD → 0-cost skip)
scan_repo ──▶ author ──▶ gen_indexes ──▶ validate_wiki ──▶ gate
gate ──converged──▶ mark_issue_for_review ──▶ update_cache ──▶ done
gate ──more work──▶ author             (bounded continuation loop; validator feedback)
```

- **scan_repo** (tool) — survey + wiki detection + the incremental
  git-head noop gate (persistent out-of-tree cache).
- **author** (claude_code agent) — the one adaptive step: plan + write the
  OKF pages, commit in stride. Guided by the two bundled skills.
- **gen_indexes** (tool, deterministic) — regenerate every directory
  `index.md` from page frontmatter. The agent never hand-writes indexes.
- **validate_wiki** (tool, deterministic) — the truth oracle: fails on
  invalid frontmatter, a dead intra-wiki link, or a write outside `wiki/`.
- **gate** (compute) — `converged = wiki_complete ∧ frontmatter_valid ∧
  links_ok ∧ scope_ok`.

## Usage

```sh
# Bootstrap / refresh a wiki for the current repo
iterion run bots/wiki-gen/main.bot --var workspace_dir="$PWD"

# Custom output dir, operator brief
iterion run bots/wiki-gen/main.bot \
  --var wiki_dir=docs/wiki \
  --var scope_notes="emphasize the runtime engine and the DSL"

# Cheaper model for large repos
ITERION_WIKI_MODEL_CLAUDE=claude-sonnet-5 iterion run bots/wiki-gen/main.bot
```

Key vars: `wiki_dir` (default `wiki`), `code_scope_globs` (empty = whole
workspace), `scope_notes` (a hint, not a scope limit), `max_passes`
(default 3), `wiki_cache_path` (persistent noop cache), `okf_version`.
Model/effort override via `ITERION_WIKI_MODEL_CLAUDE` /
`ITERION_WIKI_EFFORT_CLAUDE`.

Runs in a `worktree: auto` — pages land on a persistent branch, the
operator's checkout stays untouched.

## Skills

- [wiki-authoring.md](skills/wiki-authoring.md) — the operating playbook
  (discovery, plan-before-write, grounding, surgical update, boundaries).
- [okf-format.md](skills/okf-format.md) — the OKF frontmatter + link-graph
  contract the validator enforces.

## Roadmap

- Marker-block co-maintenance of a root `AGENTS.md`/`CLAUDE.md` pointer
  (`<!-- WIKI:START --> … <!-- WIKI:END -->`) so coding agents are told to
  consult the wiki, without clobbering hand-authored content.
- A `wiki/INSTRUCTIONS.md` operator brief that is read for scope but never
  regenerated.
- Per-page finer noop (page-sha cache is already persisted).
