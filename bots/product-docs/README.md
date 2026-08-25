# product-docs (Prody)

Functional documentation bot — one capable agent + a mission + **truth
gates only**. It writes and maintains the **business-audience**
documentation of a product ("what it does for its users") inside a
**dedicated documentation repository**, grounded in the source code of
the **N other repositories** a product catalog names.

- **Audience first**: the pages are read by the people who USE the
  product — agents, managers, citizens, partners — not by the people who
  build it. Code, schemas, endpoints, environment variables, deployment
  and architecture are all out of scope, by construction.
- **Cross-repo by construction**: the docs live here, the code lives
  there. `catalog_ingest` shallow-clones every source repo into an
  out-of-tree scratch dir and redacts credential-bearing files from the
  clones before the agent may read them.
- **Sourced or `[à confirmer]` — never invented.** Every factual claim
  is grounded in something read in a source clone, or in human-validated
  prose already on the page. There is no third option.
- **Human-validated prose is preserved**, and touched only where the
  code contradicts it or a documented journey is incomplete.
- **A repository it could not read is a hole it declares**, in the
  report and in the PR body — never an area documented from inference.

Each aligned page lands in stride (`docs(<page>): …` plus a `Bot:
product-docs` trailer **and** a `Product-Docs-Sources: <repo>@<sha>`
trailer). Claims the product is meant to honour but the code does not
implement yet are neither deleted nor aligned down: they go to a
cross-pass promises ledger and are reported under "Points à confirmer
avec l'équipe produit".

## The three documentation bots

All three write `.md` and commit. Settle the **audience** first; the
topology follows.

| Bot | Audience | Where the pages live | What it does |
|---|---|---|---|
| `docs-refresh` (Doki) | developers | the code repo, in place | aligns the repo's **existing** technical docs against the code |
| `wiki-gen` (Wikky) | developers | the code repo, `wiki/` | **generates** a navigable OKF wiki it owns |
| `product-docs` (Prody) | the product's **users** | a **dedicated docs repo** | writes the **functional** documentation from N **other** repos |

Prody is the only one that clones sources, and the only one whose output
a non-developer is expected to act on.

## Editorial sovereignty

The editorial line does **not** live in this bundle.

1. **`<workspace>/.product-docs/*.md`** — the docs repository's own
   editorial skills (documentary model, allowed blocks, glossary, tone).
   **Authoritative**: where they disagree with the bundle, they win.
   `scan_hints` reports the overrides in force on every pass, and the
   scope gate **excludes** that directory so the bot can never rewrite
   the charter that governs it.
2. **The bundle's `skills/`** — a generic default, in French (the
   published pages are French and end-user facing), used for whatever
   the docs repo did not specify.

What stays non-overridable is integrity, not taste: sourced facts or
`[à confirmer]`, preserved human prose, declared holes, and the four
`page_lint` rules. A docs repo may restyle every page; it may not
authorise the bot to invent. See
[ADR-091](../../docs/adr/091-product-docs-editorial-sovereignty-and-git-native-source-deltas.md).

## Shape

```
catalog_ingest ─▶ scan_hints ─▶ campaign ─▶ scope_check ─▶ page_lint ─▶ gate
gate ──(converged)──▶ mr_gate ─▶ forge_auth_probe ─▶ finalize_mr ─▶ done
gate ─────────────────▶ scan_hints          (continuation_loop, max_passes)
```

- **`catalog_ingest`** (deterministic, once, outside the loop) — resolves
  the product from the catalog, clones its source repos out of tree,
  redacts secret-bearing files, and emits a per-repo inventory. A repo it
  could not clone is present as a `degraded` entry **with a reason**,
  never absent. A missing catalog, an unknown product or an entry without
  `docs.product_dir` fails the run **loudly**: documenting the wrong
  directory is worse than not running.
- **`scan_hints`** (deterministic, advisory) — dead intra-doc links and
  anchors, orphan pages (in a hub-and-step model an unlinked page is
  invisible), catalog surfaces no page covers, empty pages, the editorial
  overrides in force, and the incremental base. Help the campaign is free
  to contradict — never a gate, never an obligation.
- **`campaign`** — one adaptive `claude_code` agent, `session: fresh`.
  Reads the source clones (i18n catalogs, templates, forms, routes, API
  contracts — any framework, no per-framework parser anywhere in the
  DSL), writes/repairs pages, commits each one in stride.
- **`scope_check`** (deterministic truth gate) — the writeable set is
  `<product_dir>/**/*.md` and nothing else: not the source clones, not
  the docs repo's editorial skills, not another product's directory.
- **`page_lint`** (deterministic truth gate) — a published page carries
  no working notes: no HTML comments, no "Sources" box or section, no
  "Points à clarifier" section, no "Correspondance technique" annex. A
  violation is **not converged** and the located failures feed the next
  pass.
- **`gate`** — `converged = scope_ok ∧ lint_ok ∧ docs_aligned`. Nothing
  else; the hint counts are telemetry, never conditions.

A documentation-only change cannot break a build, so there is no build
gate — `page_lint` is this bot's equivalent truth oracle on the artifact
it actually ships.

## The product catalog

`catalog_path` is either a YAML/JSON **file** (one product, or a
top-level `products:` map) or a **directory** holding one
`<product_id>.yml` per product. Relative paths resolve inside the
workspace.

```yaml
id: demo
docs:
  product_dir: documentation_produits/demo   # the writeable set
  surfaces:                                   # optional, advisory
    - name: Espace gestionnaire
    - name: Espace citoyen
gitlab:
  host: gitlab.example.org                    # host for gitlab_path entries
repos:
  - id: demo-api
    github_repo: org/demo-api                 # → https://github.com/org/demo-api.git
  - id: demo-front
    gitlab_path: group/demo/front             # → https://<gitlab.host>/group/demo/front.git
  - id: demo-batch
    url: https://forge.example.org/x/y.git    # explicit url wins over both
    ref: main                                 # optional branch/tag
```

A `.json` catalog needs no dependency at all. A YAML catalog is parsed
with PyYAML when the interpreter has it, otherwise with `yq` (declared
in this bundle's `devbox.json`); with neither, the run fails with a
precise message rather than guessing.

## Incremental mode is git-native

Every commit records which source commits it was written against:

```
Bot: product-docs
Product-Docs-Sources: demo-api@a1b2c3d4e5f6,demo-front@0f1e2d3c4b5a
```

`catalog_ingest` reads the newest such trailer from the **docs repo's own
history** and diffs each fresh clone from it. There is no side-car state
file, so a crashed run, a wiped scratch dir or a fresh cloud pod loses
nothing. When a shallow clone cannot reach a recorded commit the entry is
marked `delta unavailable` with the reason — a delta that could not be
computed is **never** reported as an empty one.

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `catalog_path` | **required** | Product catalog: a YAML/JSON file, or a directory holding `<product_id>.yml` |
| `product_id` | **required** | Which product to document (selects the entry and its `product_dir`) |
| `scope_notes` | `""` | Operator attention pin |
| `mode` | `full` | `full` = whole-product sweep against the whole source corpus; `incremental` = scoped to the source delta (from the `Product-Docs-Sources:` trailer) and the docs delta (from the `Bot: product-docs` trailer) |
| `diff_since` | `""` | Explicit incremental base on the docs side. Usually empty — `incremental` auto-detects it |
| `editorial_dir` | `.product-docs` | Where the docs repo publishes its own AUTHORITATIVE editorial skills. Empty disables the override |
| `clone_depth` | `1` | Shallow-clone depth for the source repos; `0` = full clone (raise it when a deep incremental base is needed) |
| `secret_globs` | credential-carrier globs | Files deleted from every source clone before the agent may read them |
| `lint_rules` | all four | Editorial rules `page_lint` enforces — drop a name to disable that rule |
| `extra_forbidden_headings` | `""` | Extra heading titles a published page must never carry |
| `max_hints` | `120` | Cap on the advisory hints list (context bound) |
| `dismissed_path` | `${PROJECT_SCRATCH_DIR}/product-docs/dismissed.json` | Dismissals ledger (cross-pass memory) |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/product-docs` | Out-of-tree scratch: the source clones + the promises ledger |
| `max_passes` | `4` | Continuation-loop cap |
| `open_mr` | `false` | Push the page series + open ONE PR at the end |
| `mr_draft` | `true` | Open that PR as a **draft** — human validation happens on the forge |
| `mr_branch` / `mr_base` | `""` | PR branch (default `iterion/product-docs/<run-id>`) / base |
| `source_issue_ref` | `""` | Issue to back-link the PR URL onto (forge URL or `native:<id>`) |

## PR finalization (opt-in)

`open_mr=true` appends the PR tail: a deterministic `forge_auth_probe`
checks for a push credential (mounted `forge_token` secret, `*_TOKEN`
env, or host `gh` auth) and only then the `finalize_mr` agent pushes the
page series and opens one PR (GitHub `gh` / GitLab `glab` / Forgejo REST,
per the bundle's `forge-mr-create` skill), reporting `drift_remaining`
and `unread_sources` honestly in the body plus a "Points à confirmer avec
l'équipe produit" section when the promises ledger has entries. Without a
credential the tail skips cleanly and the commits stay on the run's
storage branch. This is the delivery path for **cloud** runs, whose
runner clone is ephemeral.

**The PR is a draft by default.** Functional documentation is validated
by the product owners on the forge, and marking it ready is their act,
not the bot's. If a provider or CLI cannot open a draft, the PR is opened
anyway and the reason is reported in `skipped_reason` — the `draft`
output says whether the PR **is** a draft, not whether one was asked for.
Set `--var mr_draft=false` for a repo whose review flow does not use
drafts.

## Run

```bash
iterion run bots/product-docs/main.bot \
  --var catalog_path=catalog \
  --var product_id=demo \
  --var scope_notes='Le nouvel espace gestionnaire n'"'"'est pas documenté'
```

Add `--var open_mr=true` to open the (draft) pull request at the end, and
`--var mode=incremental` for the scheduled weekly run.

Skills shipped: `product-docs` (the operating playbook, English) plus the
French editorial defaults `modele-documentaire`, `blocs-gitbook`,
`glossaire-produit` and `ton-et-style`, and `forge-mr-create` for the
opt-in PR tail — 6 skills total. See [main.bot](main.bot) for the full
DSL.
