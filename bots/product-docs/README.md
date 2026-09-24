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
[ADR-092](../../docs/adr/092-product-docs-editorial-sovereignty-and-git-native-source-deltas.md).

## Shape

```
catalog_ingest ─▶ scan_hints ─▶ campaign ─▶ scope_check ─▶ page_lint
               ─▶ coverage_check ─▶ gate
gate ──(converged)──▶ mr_gate ─▶ forge_auth_probe ─▶ finalize_mr
                                          ─▶ surface_pr_link ─▶ done
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
- **`coverage_check`** (deterministic truth gate, armed only with a net)
  — the exhaustiveness gate. See below.
- **`gate`** — `converged = scope_ok ∧ lint_ok ∧ coverage_ok ∧
  docs_aligned`. Nothing else; the hint counts are telemetry, never
  conditions.

A documentation-only change cannot break a build, so there is no build
gate — `page_lint` is this bot's equivalent truth oracle on the artifact
it actually ships.

## The exhaustiveness gate

*Did I document everything?* is the one claim an agent cannot honestly
make about itself, and until now it rode on `docs_aligned`. When the
product carries a **golden-master net**, `coverage_check` reads it and
decides instead.

It arms **if and only if** `catalog_ingest` finds BOTH
`<oracle_dir>/feature-coverage.json` (the inventory: covered features
with the corpus entries that exercise them, exclusions with their
written reason) and `<oracle_dir>/corpus.json` (the captured entries),
in the docs workspace **or** in a source clone. Half a net is no net.
**Without one the node is inert** — `coverage_ok` true, empty log,
nothing added to the gate or to the next pass's feedback: the bot is
exactly the one it was.

With one, it refuses five ways, each cause **named**, never counted in
silence:

| cause | what it catches |
|---|---|
| `PHANTOM_DOC` | a documented screen the net never saw: a cited corpus entry that does not exist, an inventory id nobody inventoried, a path that is neither a declared route nor a corpus entry path, a query parameter the corpus never observes on that path |
| `GAP` | a covered feature no block documents — **one** line must **cite** its identifier **and** one of its own entries, **and** the block carrying it (a paragraph, a list item, a table row) must READ: prose outside the citations, `coverage_min_prose` characters of it **for each** feature that block documents. Co-presence is an index row, a neighbour's prose is not the row's own, and a heading documents nothing |
| `CONCEALED_EXCLUSION` | an exclusion the pages do not name *as* one, under the declared exclusions chapter, in a block of prose of its **own** (a bare identifier or a `TODO` is silence under a label; a row of dots is length without words; one paragraph cannot answer for two holes, and the prose has to share vocabulary with the reason the net records) |
| `UNANCHORED_CHAPTER` | a chapter (heading level ≥ 2) citing no reference **in its heading line itself** — a citation in the chapter body does not anchor it — and not declaring that it restitutes none; plus the ceiling, `coverage_max_anchorless`, on how many chapters may declare it at all |
| `NET_UNREADABLE` | the material cannot be judged: absent or unparsable artifacts, an inventory that contradicts itself, and **every emptiness** — no page, no feature, no corpus entry, an empty route table |

That last row is the point of the design, not a detail: a guard written
`if collection and …` is *disabled* exactly when the collection is
empty, so each emptiness is decided out loud instead.

**A cause whose repair lies outside the writeable set is a PRE-FLIGHT
failure, never a term of convergence.** `scope_check` lets the campaign
write `<product_dir>/**/*.md` and nothing else, so a complaint about the
net, about a launch var or about the catalog is an order it is
*forbidden* to obey: left in `fail_log` the two gates contradict each
other and the run burns every pass. `coverage_check` therefore **stops
the run**, naming every such cause, and the operator repairs the net
where the net lives. The documentation-side refusals — a gap, a phantom,
a concealed exclusion, an unanchored chapter, an empty product tree —
stay convergence terms, because their repair is a `.md` file the
campaign may write.

An **absent** `exclusions` key is an empty list, which is how the net
producer itself reads it (`coverage.get(key) or []`): a product with no
hole to declare writes no key. Only a key that is *there* and mistyped
is a refusal.

What the gate deliberately does not assume:

- **Language.** The exclusions chapter token and the anchorless marker
  are declared identifiers (`coverage_exclusions_heading`,
  `coverage_no_anchor_marker`), not French literals. `page_lint` exempts
  exactly the declared marker from its `html_comments` rule, so the two
  gates can never order the campaign to add and to remove the same
  characters. The exemption is scoped twice over: it applies to that one
  rule and to a comment that *is* the token (a marker declared
  `password` never silences the secret scan), and it is armed by the
  **net**, not by the var — with no net there is no second gate to
  contradict, and `page_lint` is byte for byte, value for value, the
  node it was before this gate existed.
- **What a reference IS.** Nothing is inferred, in either direction. A
  reference is what the page *says* is one —
  `coverage_citation_open` + token + `coverage_citation_close`,
  `[[ref:001]]` by default — and a code span is prose, whatever it looks
  like. So every citation is verified **wherever it sits**, with nothing
  else on its line (an invented screen described one sentence at a time
  is refused), and nothing that is not a citation is ever looked at (an
  interface label, a file name, an inline `404` stay prose). The token
  is a corpus entry id, an inventory feature id, or a path (leading
  slash); anything else the net does not hold is a `PHANTOM_DOC`.

  This replaces a shape inference — `(length, character classes)` — that
  two review rounds could not settle: tightened, it let an invented
  reference alone on its line through; loosened, it refused
  `` `Mot de passe` `` and `` `package.json` `` as corpus entries that do
  not exist, and since this gate is a convergence term the campaign was
  ordered to delete reader-facing prose. **There is no setting that
  closes both directions**; an explicit marker has no middle. Both ends
  of the syntax are declared, so a docs repo already using `[[…]]` picks
  another spelling; either one empty is a refusal that stops the run.
- **What a block IS.** Prose is credited over the block that carries
  the citation, and a block is what markdown renders as one — not a run
  of non-blank lines. A table row (leading pipes or not), a list item, a
  line of block-level HTML and a heading are each read on their own; a
  quote marker, a thematic break and a `{% … %}` template line end the
  block before them; an underlined title is a heading. Table pipes, HTML
  tags and link destinations are markup, not prose. A heading documents
  nothing: it labels and anchors a chapter. And a block that documents
  several features **shares** its prose between them — each needs its
  own `coverage_min_prose`. Lumped into one paragraph, an index table
  with a single descriptive column credited its header and every other
  row to each feature listed in it; and a paragraph of soft-wrapped index
  lines — no markup at all — was credited whole to each feature it
  listed. A row that really describes its feature still documents it, by
  its own cells.
- **A catch-all route.** A route made only of placeholders (`/{slug}`,
  `/**`) matches every path and proves none: for a path only such a
  route covers, the corpus reference is the only evidence. The rule cuts
  both ways — a *cited* path made only of placeholders is refused too,
  since a citation is read as a pattern and a lone `` `/**` `` would
  otherwise match every corpus entry while restituting nothing. A **tail
  wildcard** is the same claim wearing a literal segment:
  `` `/dashboard/**` `` clears the placeholders-only rule and still
  matches every screen below it. One predicate governs both sides — what
  the gate refuses a page to cite, it refuses a route table to prove.
- **The exclusions chapter title.** The declared token matches the
  **whole** heading, anchored, the way `page_lint` matches its own
  chrome headings. As a substring, an insurance product's
  reader-facing "Les exclusions de garantie" opened the chapter of
  documented holes.
- **Where the route table lives.** `coverage_routes_file` is read from
  inside the net directory; an absolute path or a `..` escape is a
  refusal, like `oracle_dir`.

**The route table is a degradation, and a loud one.** golden-master
states its routes through `config.json`'s `routes_probe` — a command it
replays at every gate — and commits **no artifact** for them; its
`route-coverage.json` carries only the justified exclusions. So
`coverage_routes_file` usually names a file that is not there, the path
check falls back to the corpus alone, and the gate says so in its log
and in `routes_degraded`. Degraded still **refuses**: a cited path that
matches no corpus entry path is named a `PHANTOM_DOC` with the
degradation attached. Lifting it takes one artifact on the
golden-master side — the `routes_probe` stdout committed as
`<oracle_dir>/routes.txt`.

**The escape hatch has a ceiling, and the refusal knows it.**
`coverage_max_anchorless` (default **2**, what the bundle's own
editorial model needs) caps how many chapters may declare they
restitute nothing; above it the declarations are themselves the
refusal. The anchor refusal offers the marker **only while headroom
remains** — otherwise it asks for an anchor and says the hatch is full.
A refusal that named a remedy the ceiling then took back made the two
causes ping-pong until `max_passes`, and the ceiling's own remedy is a
launch var the run may not write, so the refusals never ask for one. The
chapter that **opens** the declared exclusions chapter is anchored by
its role: it needs no marker and spends nothing.

The counts are not private to the gate either. `counts_line` —
features documented, exclusions named, chapters declared anchorless and
the ceiling in force — is an output field, so it rides the run events,
and `finalize_mr` quotes it verbatim in a **Couverture** section of the
PR body. A declaration nobody ever reads is free, which is exactly what
it must not be.

**The net itself is read as given.** Nothing here proves the net's
*own* gate ever ran on it. When neither `verify-oracle.sh` nor
`REPORT.md` sits beside the two artifacts, the gate says so
(`net_unproven`) the way it says `routes_degraded` — and even when they
do, their presence is a trace, not a verdict: neither file is stamped
with the commit it judged. The ask on the golden-master side is one
artifact carrying that stamp, alongside the `routes_probe` output.

What it does **not** judge is the prose. That stays with its reader.

## The product catalog

`catalog_path` is either a YAML/JSON **file** (one product, or a
top-level `products:` map) or a **directory** holding one
`<product_id>.yml` / `.yaml` / `.json` per product. Relative paths
resolve inside the workspace.

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
    url: https://forge.example.org/x/y.git    # explicit url wins over all
    ref: main                                 # optional branch/tag
  - id: demo-self
    path: .                                   # a repo already on disk, RELATIVE
                                              # to the workspace; `.` = the
                                              # workspace itself
```

`path` is the form a **campaign** generates to document **its own**
repository: the source is cloned over the filesystem — no forge, no
network, no credential — and then redacted and read exactly like any
other source. Precedence is `url` > `path` > `github_repo` >
`gitlab_path`, and the inventory reports which decision each entry took
(`local: true` = read from the filesystem).

**A source that names the FILESYSTEM takes ONE decision, whichever key
carries it.** A `url` whose value is an absolute path, a `file://` url or
a relative value that resolves on disk is a local source under another
name — it is read with the same containment, the same credential-free
environment and the same clone mode as `path`. The catalog is repo
content, so that containment is:

- confined to the workspace (`.` = the workspace itself); an absolute
  path under `path`, a `..` escape, a symlink out, or a value outside the
  workspace under `url` is a named `degraded` entry;
- the root of a repository, never a plain directory;
- no **delegated object store**: a repository whose git dir, object
  directory or `objects/info/alternates` resolves outside the workspace
  is refused. A clone serves the union of those stores, so confining the
  path alone confines nothing — and `--no-local` does not change that,
  since `upload-pack` serves the alternates too.

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
| `oracle_dir` | `.golden-master` | Where the golden-master net lives, looked up in the workspace then in each source clone. Both `feature-coverage.json` and `corpus.json` present ⇒ `coverage_check` is armed; empty disables the lookup |
| `coverage_exclusions_heading` | `exclusions` | Title a heading must carry, WHOLE and anchored, to open the chapter under which an exclusion counts as NAMED (case- and accent-insensitive) |
| `coverage_no_anchor_marker` | `<!--no-anchor-->` | What a chapter carries to declare it restitutes no reference. `page_lint` exempts exactly this token, and only when a net is present; empty disables the escape hatch |
| `coverage_max_anchorless` | `2` | Ceiling on the chapters that may declare they restitute nothing. The anchor refusal offers the marker only while headroom remains. The exclusions chapter is anchored by its role and never counts |
| `coverage_routes_file` | `routes.txt` | Declared route table inside `oracle_dir` (relative, no `..`), in the golden-master `routes_probe` grammar. Absent ⇒ the path check degrades to the corpus and says so |
| `coverage_citation_open` / `coverage_citation_close` | `[[ref:` / `]]` | The citation syntax. A reference is what the page says is one; a code span is prose. Both are declared so a repo already using `[[…]]` can pick another spelling; either one empty is a refusal that stops the run |
| `coverage_placeholders` | `TODO,FIXME,…` | Substitutes that do not count as writing when a page documents a feature or names an exclusion |
| `coverage_min_prose` | `60` | Minimum prose characters, outside the citations and the markup, in the block (paragraph, list item, table row) naming an exclusion or documenting a feature — for EACH feature that block documents |
| `dismissed_path` | `${PROJECT_SCRATCH_DIR}/product-docs/dismissed.json` | Dismissals ledger (cross-pass memory) |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/product-docs` | Out-of-tree scratch: the source clones + the promises ledger |
| `max_passes` | `4` | Continuation-loop cap: the loop back to `scan_hints` is taken at most this many times, so a run makes up to `max_passes + 1` campaign passes |
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

## Publication (opt-in)

`publish=true` appends a publication tail after the PR tail: a
deterministic `publish_gate` (opt-in + `publish_base_url` + `publish_image`
+ both credentials mounted + a `deploy-target` skill actually mirrored into
the workspace, else the tail is skipped with its reason), the `publish`
agent, and a deterministic `verify_publish` truth gate that polls the live
URL from outside the agent's narrative and FAILS the run when the site is
not serving under `publish_base_url`.

The agent follows two skills. The bundle's own `publish-static-site.md`
builds the docs into a MkDocs Material site (`deploy/gitbook_to_mkdocs.py`
converts the GitBook blocks) and packages it with `crane` as one layer on
`nginxinc/nginx-unprivileged` (port 8080, uid 101), pushed to
`publish_image:<docs commit>` with the `registry_token` secret (stdin
login, never argv, and a `DOCKER_CONFIG` scoped to the scratch directory so
the login does not outlive the run). The operator-attached **`deploy-target`**
skill — a plugin or library skill resolved by name, exactly as app-dev's
deploy phase — then runs that image, BY DIGEST (`publish_image@sha256:…`,
resolved from the registry after the push: the docs-sha tag is a mutable
alias, and an unchanged tag is an unchanged pod spec, so a rebuilt site
would never roll out), with the `deploy_credential` secret by reference,
and returns the URL. No platform
literal lives in the DSL: swap platforms by attaching another
`deploy-target` skill and credential. `bots/product-docs/deploy/` also
keeps `onyxia-serve-init.sh`, the standing-service half of the former SSP
Cloud datalab target, for the plugin that still uses it.

## Run

```bash
iterion run bots/product-docs/main.bot \
  --var catalog_path=catalog \
  --var product_id=demo \
  --var scope_notes='Le nouvel espace gestionnaire n'"'"'est pas documenté'
```

Add `--var open_mr=true` to open the (draft) pull request at the end.
The weekly scheduled invocation already carries `mode: incremental`
(`manifest.yaml`); pass `--var mode=incremental` by hand for an
out-of-band incremental pass.

Skills shipped: `product-docs` (the operating playbook, English) plus the
French editorial defaults `modele-documentaire`, `blocs-gitbook`,
`glossaire-produit` and `ton-et-style`, and `forge-mr-create` for the
opt-in PR tail — 6 skills total. See [main.bot](main.bot) for the full
DSL.
