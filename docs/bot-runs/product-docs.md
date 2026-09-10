[← Bot runs](README.md)

# product-docs (Prody) — run bilans

Functional, business-audience product documentation generated and maintained
in a dedicated docs repository from a multi-repo product catalog. Newest run
first.

## 2026-09-04 — first run on the prod instance: the datalab URL brought back, then re-published by the bot (run 01a06baa)

- Status: **validated on prod** — first `product_docs` run ever on the cloud
  instance (every earlier run was local), scope ✓ lint ✓ converged,
  publish tail end to end, `verify_publish` **http 200** from outside.
- Versions: bot 1.0.0 (baked catalog) · iterion prod v3.100.0 runner /
  v3.100.1 server · `claude_code` / `claude-opus-5` on the team forfait.
- Method: repo-targeted launch on `DNUM-SocialGouv/documentation_produits`
  @ `demo/prody-sirena` (the only branch carrying `.prody/catalog/`),
  `max_passes=0`, `open_mr=false`, `publish=true`, `publish_base_url` = the
  RELAUNCHED datalab service, `publish_s3_bucket=devthejo`,
  `publish_tools_ref=v3.100.0`. Team secrets `onyxia_s3_*` (renewed STS
  tokens) + `forge_token` (the PAT connection's managed secret — the docs
  repo has no repo integration, so the clone credential comes from the
  bot binding) bound to `product-docs`.
- Result: 44 min, **$25.5** — campaign $25.0 (one verification pass that
  fanned out 49 sub-agents in two modalities, page-first then
  source-first), publish $0.5 (105 s). 3 commits on 2 pages, all real:
  the fourth distribution and two national key figures of the
  indicators dashboards, a third filter that silently removes requests
  (`Inclure les EIG`), the silent merge and the non-shared reopening of
  a request. `is_product_bug: true` (filed in the summary — the board
  MCP is unavailable in the sandboxed runner session, as for Doki).
  The site was rebuilt (21 pages) and mirrored; the truth gate polled
  the new host and got `http 200, title present`.
- Delivery: with `open_mr=false` the 3 commits are BANKED on
  `iterion/run-01a06baa…` in the DNUM repo (the engine's death/finish
  bank works on a foreign repo too) and nothing reached the draft PR #24
  — a deliberate choice for a "verify + publish" run, but it means a
  human cherry-pick or a follow-up run with `open_mr=true`.

### What the run says

- **The URL had died with the platform, not with the bot.** On 09-02 the
  STS tokens expired and the datalab culled `prody-docs-serve`; the bucket
  still held the site. Relaunching the service (Vscode-python, init
  script pinned at `v3.100.0`, `SITES_BUCKET=devthejo`, user port 5000)
  brought `/sirena/` back to 200 within a minute, BEFORE any run — a new
  release id, hence a new host. The launcher takes its whole config as
  URL parameters (`init.personalInit=«…»`, `extraEnvVars[0].name=«…»`,
  `networking.user.enabled=true`); the brackets must stay literal, a
  percent-encoded `%5B0%5D` becomes a bogus top-level key.
- **A manual launch runs on the launcher's OWN forfait first.** The first
  attempt (`01a06b9f`, `POST /api/runs` with the operator token) died in 6 s
  on `usage cap: seven_day window at 97%` — the operator's personal
  Claude connection, at its weekly ceiling, sits ABOVE the team key in the
  credential tiers. Relaunching through a one-shot schedule row (a system
  actor) resolved the team key and ran. Worth knowing before any manual
  prod launch on a shared team.
- **Scope notes are read by the campaign, not by the tail.** The notes
  said "verification pass + publication"; the campaign agent took
  "publication" as its own job, mirrored the site to the bucket itself
  and probed the OLD hostname it found in the repo (404 — the culled
  service), then asked for an operator action the publish tail had
  already made moot. Harmless here, but a pass spent on the tail's work:
  keep scope notes about the DOCS.
- **The verification pass is no longer a $5 pass.** The campaign
  self-orchestrates (49 sub-agents) at the bot's default effort; the local
  recipe ran it at `high`. On a converged corpus that is the price of
  breadth — the two findings Round 2 surfaced were in areas a page-first
  reader never opens.
- **Two credential leaks, both mine, both in the transcript**: a full
  accessibility snapshot of the datalab launcher renders the helm values
  preview WITH the service's temporary credentials, and a value-stripping
  filter written for `key=value` lines printed the `key = value` ones. The
  tokens were renewed and the bot only ever saw the new ones; the old ones
  expire on 09-11. Extract by filtered `evaluate`, never by snapshot, and
  redact by printing token COUNTS, never fields.

### Lessons for next run

- The datalab is best effort by design (7-day STS, 7-day cull): the demo
  survives only as long as someone relaunches. Prody 1.1.0 makes the tail
  platform-agnostic (image + `deploy-target`); the durable target is
  fabrique.
- Launch prod dogfoods through a one-shot schedule row, delete it after
  the tick; keep `open_mr=true` when the pass is allowed to write, or the
  commits stay on the bank branch.

## 2026-08-26 — first real product + first publication: Sirena live on the SSP Cloud datalab (run 01a03ed3)

- Status: **validated** — the whole chain ran once on a real product,
  including the opt-in publication tail shipped the same day (#533).
- Versions: bot 1.0.0 (publication tail on branch, merged as #533 the next
  morning) · iterion v3.64.1 (release binary) · skill
  `deploy-onyxia-sspcloud` v0.1.0 → v0.1.3 during the session.
- Method: local run from the DNUM docs repo
  (`DNUM-SocialGouv/documentation_produits`, branch `demo/prody-sirena`),
  `claude_code` / `claude-opus-5`, `max_passes=0` (one verification pass:
  the corpus had converged on 08-23/25, 22 pages), `open_mr=true`,
  `publish=true`, `publish_base_url` = the standing datalab service's
  user-port host, `publish_s3_bucket=devthejo`. Onyxia S3 STS credentials
  in the local sealed store (three file secrets).
- Result: scope ✓, lint ✓ → `publish_gate` `ready` → the publish agent
  built the site (GitBook blocks → MkDocs Material through
  `deploy/gitbook_to_mkdocs.py`), mirrored it to `s3://devthejo/sites/sirena`
  and reported `deployed: true` → `verify_publish` **http 200 from outside**
  → `surface_site_link`. ≈ $17, 25 min. The six verification commits of the
  pass were cherry-picked onto the draft PR #24 in the DNUM repo (kept in
  draft: merging is the product owners' act).
- Value: the DNUM deliverable — a business-readable Sirena documentation
  generated from the sources, verified, **served on a URL the bot itself put
  live**, with the RGPD finding (three retention periods documented, one
  implemented) kept in the promises ledger instead of aligned down.
- Findings / misses:
  - Two runs before this one finished WITHOUT opening their PR because a
    file secret that was not mounted stays silent (`optional: true`);
    fixed engine-side (#531), and the bot-probe recipe (a `tool` node
    testing `{{secrets.X.path}}`) stays the way to check a mount.
  - `MC_HOST` is parsed as a URL: an STS secret carrying `/` silently
    truncated the authority. Percent-encode every part (skill v0.1.3).
  - `mc`'s table renders on failure too; only the exit code is truth (a
    `| tail -1` had swallowed it once).
  - The datalab bucket does not pre-exist (`mc mb --ignore-existing`
    first), `diffusion/` is authenticated-read only (public exposure goes
    through a datalab service on the user port).
- Engine hardening: #531 (unmounted file secret fails loudly), #526
  (sandbox default image falls back to `:latest` between releases).
- Lessons for next run:
  - The platform is best effort by design: STS credentials expire in ~7
    days and the datalab may delete the serve service on the same horizon.
    Both happened on 2026-09-02 — the URL 404'd by itself. A bot that
    publishes weekly needs a durable target (fabrique) or a rotation.
  - A plugin-source manifest is not validated at registration: an
    unquoted `: ` in `plugin.yaml` broke EVERY webhook launch of the
    instance for 2h20 (#536–#538). Validate the YAML before registering.

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
