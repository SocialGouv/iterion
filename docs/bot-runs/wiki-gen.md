[← Bot runs](README.md)

# wiki-gen (Wikky) — run bilans

Wikky generates and incrementally maintains a navigable, Open-Knowledge-Format
wiki for any repository. See [the bot README](../../bots/wiki-gen/README.md) and
its skills (`wiki-authoring`, `okf-format`). Newest run first.

## 2026-09-17 — profile 2: the author prompt reaches the model with its paragraphs (run 01a0af4e-f21d)

- Status: **validated** — a three-page wiki written and gated in one pass on a scratch repository.
- Versions: bot wiki-gen 1.0.2 (`dsl: 2`, wave 3a of #1344) · iterion `db8dbb8eb` for the bots; the engine the branch binary `v3.154.1+d5b7db09f` carrying the first wave-1 runtime fix (main is at v3.157.0 with both), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records · claude_code +
  claude-sonnet-5 (`ITERION_WIKI_MODEL_CLAUDE`).
- Method: CLI `iterion run <bundle>/main.bot` launched FROM a scratch copy of the greet project,
  `--store-dir` the operator's workspace store, `--sandbox none --merge-into none`,
  `--var scope_notes="A two-page wiki: the greet CLI and how it is tested." --var max_passes=1`,
  caps `--max-cost-usd 3 --max-duration 15m`, `ITERION_BIN` the branch binary.
- Result: **finished**, ~3.5 min, **$0.40** (author, 57 459 tokens): `wiki/greet-cli.md`,
  `wiki/testing.md` and the index, `wiki_complete: true`, the deterministic gate green;
  storage branch `iterion/run/boreal-snap-midnightkazoo-a44c`, merged nowhere.
- Value: the bot's graph live on profile 2 (`scan_repo` → `author` → the gate); the run's
  `events.jsonl` carries the rendered author prompt with `end to end.\n\nTHE MISSION`, the
  paragraph break the profile-1 lexer used to fold.
- Findings / misses: none on the bot. Not exercised: the incremental-update path
  (`wiki_exists` true) and a gate failure feeding a second pass.
- Engine hardening: none needed.
- Lessons for next run: sonnet is enough for a tiny repository; keep `max_passes=1` for a smoke.

## 2026-07-20 — first dogfood on iterion, self-host wiki (runs 019f7f82 + 019f7f8a)
- Status: validated
- Versions: bot 1.0.0 · iterion @ e8c1692 (base) — bot source on branch `worktree-doki-noop-gate`
- Method: `claude_code` / `claude-sonnet-5` (via `ITERION_WIKI_MODEL_CLAUDE`),
  `worktree: auto`, `--merge-into none`, `--var scope_notes="tight ~6-9 pages:
  DSL pipeline, runtime engine, backend stack, CLI"`, `--var max_passes=2`,
  `--max-cost-usd 15`, store `<repo>/.iterion`.
- Result: **converged on pass 1** (run 019f7f8a): FINISHED in 7.8m, 41 031
  tokens (sonnet, OAuth forfait → no metered $). Gate `converged: true`;
  validate all-green (7 concept pages, `frontmatter_valid` ∧ `links_ok` ∧
  `scope_ok`, 0 orphans, quickstart present). Wiki landed on storage branch
  `wikky/dogfood2 → 0fb1ca7` (operator's `main` untouched). Layout:
  `quickstart.md` + `architecture/{dsl-compilation-pipeline, runtime-engine,
  backend-execution-stack, worktree-and-persistence}.md` +
  `workflows/{authoring-bot-workflows, cli-surface}.md` + 3 generated indexes.
- Value: a genuinely useful, **grounded** navigable wiki — real file citations
  (`pkg/dsl/workflowfile/workflowfile.go` + its `Extensions`/`IsWorkflowFile`,
  `pkg/dsl/parser/lexer.go`), concept-to-concept relationship links, OKF
  frontmatter throughout, source-code links. Not a façade: every claim cites a
  read. OKF output is directly graphify-ingestible.
- Findings / misses (Wikky did well): the deterministic gates held — the agent
  could not ship a broken wiki. Coverage was intentionally tight (scoped);
  `remaining` honestly named the un-covered areas (dispatcher/triggers, cloud/
  multitenancy, sandbox) for a future pass.
- Engine / bot hardening (surfaced by the run):
  - **validate_wiki false positive on source links** (run 019f7f82, fixed in
    `06bbfdbea`): the validator treated every relative link as intra-wiki and
    appended `index.md` to trailing-slash links, so legitimate links to source
    dirs (`../../pkg/dsl/parser/`) were flagged dead — forcing a wasted
    continuation pass. Fix: split by location — links resolving INSIDE the wiki
    must hit a page/anchor; links resolving OUTSIDE are source refs, valid when
    the path exists on disk (dir or file), only a truly-missing path flagged as
    a "broken source ref". Re-verified against the real generated wiki
    (`links_ok` true) and a broken-ref case.
  - Noop short-circuit proven in the real engine on a seeded fixture:
    `scan_repo -> done (condition: noop_skip)`, zero LLM cost — "wiki already
    current for HEAD".
- Lessons for next run:
  - Under `--merge-into none` a second run does NOT noop (the wiki lives on an
    isolated storage branch, not on the base the next run starts from — so
    `wiki_exists` is false and the cache HEAD can't match). This is correct.
    The noop is designed for the production path (`--merge-into current`, where
    the wiki lands on the branch the next run re-reads) or CI (commit the wiki).
  - sonnet-5 was ample for a tight, grounded wiki at ~40k tokens / 8m. Opus
    (the default) is worth it for a large first-generation or dense subsystems;
    scope with `scope_notes` + `max_passes` to bound cost.
  - The generated wiki is inspectable at `git show wikky/dogfood2:wiki/...`.
