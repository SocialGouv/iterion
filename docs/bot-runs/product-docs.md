[← Bot runs](README.md)

# product-docs (Prody) — run bilans

Functional, business-audience product documentation generated and maintained
in a dedicated docs repository from a multi-repo product catalog. Newest run
first.

## 2026-08-25 — first dogfood, fixture product (run 01a03a6a)

- **Status:** validated (converged), with one real bot bug found and fixed.
- **Versions:** bot 1.0.0 · iterion `fd9c5e611` (worktree build).
- **Method:** `claude_code` / `claude-opus-5`, two campaign passes.
  Fixture: a docs repo (`/tmp/prody-dogfood/docs`, empty product tree, one
  local editorial override) and one source repo (`subvention-app`: a grant
  request service — FR i18n catalog, two role-scoped route files, one form
  template, a `.env`). Catalog `catalog/demo.yml` naming a single repo by
  local `url:`. Vars: `catalog_path=catalog/demo.yml product_id=demo
  max_passes=2 scratch_dir=/tmp/prody-dogfood/scratch`. Flags:
  `--merge-into none`, `--sandbox none` (no container runtime on the host),
  `open_mr` left false.
- **Result:** converged on pass 2. 21 commits, one page per commit, all
  carrying both `Bot: product-docs` and
  `Product-Docs-Sources: subvention-app@e7bb04f1`. 8 pages: product home,
  glossary, two role hubs (`espace-demandeur`, `espace-instructeur`), two
  step pages each. `page_lint` green over all 8 (0 violations); `scope_check`
  green; `gate.converged = true`. ≈$20.6 (13.6 + 7.0), ≈50 min wall clock.
  Commits landed on `iterion/run/01a03a6a…` (merge deliberately skipped).
- **Value:** high, and specifically the value the bot was built for. From an
  EMPTY product directory the campaign produced a role-then-journey page set
  a business reader can follow, in French, grounded file by file: statuses
  from `fr.json`, the SIRET/50 000 €/10 Mo constraints from the route
  validators, the "réponse sous 30 jours" promise from the mail body, the
  mandatory refusal motive from the refuse handler. Unprovable facts were
  marked `[à confirmer]` in-sentence rather than invented or dropped — pass 2
  spent all 5 of its commits doing exactly that re-grounding.
  - The **docs repo's own editorial rule was obeyed over the bundle default**:
    `.product-docs/modele.md` demanded that hub pages open with
    `## À qui s'adresse cette page`, and both hubs do — while the product home
    (not a hub, per the bundle's own taxonomy) correctly does not. Editorial
    sovereignty (ADR-092) works end to end.
  - It found a genuine **source** defect and said so instead of documenting a
    promise the code does not keep: `instruction.js` comments that accepting
    "notifies the applicant", no mailer call exists, and `fr.json` has no
    acceptance/refusal message body. Reported as `is_product_bug: true` and
    filed on the board rather than papered over.
  - It refused to resolve an ambiguity it could not prove (the worklist reads
    `deposee`, both decision handlers call `transition(id, 'instruction', …)`,
    and `transition` is defined in none of the cloned files): it documented
    both ends and marked the passage between them.
- **Findings / misses:**
  - The redaction is honest but blind by construction: the inventory reported
    "1 secret-bearing file redacted" and the campaign flagged, unprompted,
    that it therefore could not know what functional area that file covered.
    Right behaviour; worth remembering that a product whose config carries
    functional truth will be under-documented by design.
  - `catalog_ingest` failed LOUDLY and usefully three times before the run
    started (missing YAML parser path, unwritable scratch root) — each message
    named the cause and the remedy. No silent degradation observed.
- **Engine / bot hardening:** one real bug, found only because this was a live
  run. iterion mirrors the bundle's skills into `<workspace>/.claude/` at run
  start and again on every resume; `scope_check` collected untracked paths
  from `git status` and judged that engine-owned tree against the writeable
  set, so a pass that wrote 8 pages and 16 clean commits was failed on
  `.claude/`. Worse than a false positive: the agent then **deleted the
  engine's own skill mirror** to satisfy the gate. Fixed by excluding
  `.claude/` in `scope_check` (surgically — an untracked tree that is not the
  mirror still fails), with both halves pinned in
  `bots/product_docs_gates_test.go`. docs-refresh escapes the same class only
  by accident: its writeable set is "any `.md`", which happens to admit the
  mirrored skills.
- **Lessons for next run:**
  - Budget the campaign properly: `--max-cost-usd 15` killed pass 1 at
    `scope_check` after the agent had already spent $13.57 and committed
    everything (the 90% hard limit blocks the next node). A single Prody pass
    on a one-repo product costs $7–14; budget `max_passes × ~15` or leave the
    bot's own 300 ceiling alone. Resuming with `--max-cost-usd 45` picked up
    exactly at `scope_check` and lost nothing.
  - `iterion resume` has no `--sandbox` flag (only `iterion run` does) — use
    `ITERION_SANDBOX_DEFAULT=none` on a host with no container runtime.
  - Two passes is the natural shape on a bootstrap: pass 1 authors the page
    set, pass 2 re-grounds it. `max_passes=2` was enough here; a multi-repo
    product will want the default 4.
  - The fixture is worth keeping: an empty product tree + one small
    role-shaped source repo exercises hub/step authoring, glossary,
    `[à confirmer]` discipline and the editorial override in under an hour.
