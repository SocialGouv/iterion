# app-dev (Appy) — dogfood runs

Newest first. Template: see [README.md](README.md).

## 2026-07-21 — the delivery pipeline, proven except the pull (runs 019f8384, 019f8425)
- Status: **partial** — everything iterion controls worked; the last step is a
  GitHub platform limit, not a bug.
- Versions: bot app-dev 0.1.0 · iterion `9a2787636` → `d30f99561`
- Method: studio Launch → the **iterion-sandbox** App connection, "Create a new
  repository", `mode=autonomous`, `deploy_enabled=true`, `draft_review=false`.

### 019f8384 — a live URL that was not a delivery
Blocked from pushing CI and from publishing an image, the agent served the app
from a **ConfigMap on a stock `node:22-slim`**. The URL genuinely answered 200,
every liveness field was honest — and the repo still held only its initial
commit. Nothing versioned, nothing reproducible, gone with the namespace.

The gate passed it, because the gate only asked "is it live". That is the
lesson: **liveness is necessary, not sufficient**. `deploy_trace` now also
requires HEAD reachable from a branch on origin, the image published under the
repo's registry, and the image naming the pushed commit.

### 019f8425 — the pipeline works
```
push origin        ✓  branch appy/quote-service
CI build-and-push  ✓  success
image              ✓  ghcr.io/…/appy-quotes-ci:5df0fb99…   ← tagged with the SHA
manifests          ✓  Deployment + Service + Ingress applied
image pull         ✗  403
```

### The blocker, root-caused
- **A GitHub App installation token cannot pull a private GHCR package.** The
  docs list only classic PATs as registry credentials. Measured on one image,
  one tag: App token **403**, user PAT **200**.
- **No API changes package visibility** — four endpoint variants all 404 from CI
  with `packages: write`. It is UI-only.

So the pull secret the playbook prescribed could never have worked, and the bot
cannot fix it alone. An org-wide PAT was rejected on the right grounds: it would
let every deployed app read every other app's images. Harbor (live at
`harbor.fabrique.social.gouv.fr`) would solve it with per-project robots, at the
cost of a permanent stack element for a one-time click — declined.

**Resolution: the human gate.** The deploy phase queries the package's
visibility as soon as CI publishes, asks the operator with the exact settings
link if it is private, stays silent if it is already public, and **re-attempts
the pull rather than trusting the confirmation**. Once per application, not per
delivery. Skill v1.2.0 drops the unusable pull secret.

### Engine bugs these runs surfaced
- `d7b4db9f8` — **devbox first-class reached no catalog bot**. `bundleHost` was
  set only for a packed `.botz`, so a plain `.bot` (which is what iterion ships)
  got no bundle mount and its `devbox.json` was never read. All nine tests
  passed throughout: each supplied a bundle path and asserted the logic
  downstream of it; none asserted a real bot ever gets one.
- `d30f99561` — **the traceability gate read an unsubstituted template**. A tool
  node's command resolves `vars.*` but not `outputs.*`, so the gate judged the
  literal `{{outputs.deploy.image_ref}}` and produced false negatives — it
  failed a delivery that was correct. A gate that rejects good work is as
  damaging as one that passes a façade; it just fails in the direction that
  looks responsible.
- `e3b8f0789` — **a fresh database could not boot**. Dropping a retired index
  returned `NamespaceNotFound` (26), not `IndexNotFound` (27), so `EnsureSchema`
  errored on any empty database. Invisible in prod (the collection exists),
  fatal in CI: it turned `cloud-e2e` red for six runs while prod stayed healthy.

### Lessons for next run
- **Check the token, not the grant.** The pre-flight reported "nothing missing"
  while the minted token carried neither `workflows` nor `packages` — the
  installation had them, the token did not. The health view now reports both,
  separately.
- Equip before constraining: the agent probed for docker/podman/buildah/skopeo/
  nerdctl/kaniko and fetched a binary itself. The prompt now states what the
  sandbox has (`crane`, `yq`) and what it deliberately lacks.
- Pin bot tooling: `devbox.json` carries explicit versions and a committed
  `devbox.lock`, so a run cannot silently acquire a different binary.

## 2026-07-21 — deploy phase e2e on CLOUD prod, org-private plugin (run 019f8191)
- Status: **partial** — the app was built and verified; the deploy was blocked
  by a missing GitHub App grant, and the run wrongly reported success.
- Versions: bot app-dev 0.1.0 · iterion `e30b7daf2` (server + runner)
- Method: studio Launch → connection `iterion-forge-c6efcfed` (the
  **iterion-sandbox** App), "Create a new repository" →
  `iterion-sandbox/appy-live-quotes` (public), `mode=autonomous`,
  `deploy_enabled=true`, `draft_review=false`, `open_mr=false`.
  Brief: a tiny quote service — `PORT`/`0.0.0.0`, non-root Dockerfile,
  `/healthz`, plus a CI workflow pushing the image to the repo's registry.
- Result: converged in ~15 min. Campaign built the app, `verify_run` green
  (`✔ GET /healthz returns 200 OK`, `✔ GET /api/quote`), `review.clean`,
  `gate.converged`. Deploy **blocked**; no live URL. Local commit
  `c2fefcfc` on `iterion/run/019f8191…` — **never pushed**: `origin/main`
  still holds only the repo-creation `Initial commit`.

### What the run PROVED (all firsts, all on prod)
- **The org-private plugin reaches a cloud run through git.** The agent read
  `.claude/skills/deploy-target` from the `PluginSource`
  (`SocialGouv/iterion-deploy-msociaux@v1.0.0`, pinned). The pods had just
  been restarted, which wipes any ephemeral injection — so this can only have
  come from the durable source. ADR-079 + ADR-080 validated end to end.
- **A team can hold several GitHub Apps.** The run used the `iterion-sandbox`
  App while the SocialGouv App stayed on its own connection; git identity
  resolved to `iterion-forge-c6efcfed[bot]`. See ADR note in the commits
  below.
- **A repo iterion creates is immediately in scope.** No owner intervention,
  because the App is installed on `iterion-sandbox` with *All repositories*.

### Findings
1. **Missing App grants block the whole publish path.** The manifest requests
   `contents/pull_requests/issues/metadata/repository_hooks` (+
   `administration` when opted in). It does **not** request `workflows` or
   `packages`. GitHub refuses outright: *"refusing to allow a GitHub App to
   create or update workflow .github/workflows/ci.yml without `workflows`
   permission"* — so the CI file cannot be pushed, no image is built, and
   nothing can be deployed. `packages: write` is needed for GHCR on top.
2. **The deploy gate was an LLM and it waved a failure through.** On the
   redeploy, with `deployed=false, deployed_url=""`, the judge answered
   `pass=true, reason="Acknowledged."`. The loop exited to `done` and the run
   status read `finished`. A run that deployed nothing looked successful —
   the exact façade [workflow_authoring_pitfalls](../workflow_authoring_pitfalls.md)
   is about. **Fixed** in `0157000f5`: `deploy_verify` is now a `compute`
   gate (`deployed && healthy && deployed_url != ''`) carrying the agent's own
   notes into the retry. The agent's reporting was never the problem — its
   root-cause note was excellent; the gate was.

### Engine hardening this run produced
- `ca3d939fc` + `a9f02e425` — GitHub App keyed by **connection**
  (`Connection.OAuthAppID`) instead of `(tenant, provider, host)`; uniqueness
  moves to the owning org. Creating the replacement index does not retire the
  old one, so the legacy unique index must be dropped explicitly — it kept
  silently enforcing the old rule through a full deploy.
- `aa5eaacf3` — studio App picker; the create-App card used to be unreachable
  whenever an App already existed, i.e. exactly when a second org was needed.
- `e30b7daf2` — the clone's git credential is no longer frozen in
  `remote.origin.url`; a credential file is refreshed from the store for the
  whole run (an installation token lives 1h, this run's push comes hours in).
- `2d1064329` — the plugin-source store was never constructed; the REST
  surface answered 501 and the whole feature was dead on arrival.

### Lessons for next run
- **Grant `workflows: write` + `packages: write`** before re-running, and
  remember the runtime mint must request them too: adding them to the manifest
  alone changes nothing, because tokens are minted from
  `RuntimeInstallationPermissions()`. Minting a permission the installation
  lacks 422s, so legacy installations need an intersection (or a documented
  fallback) — do not ship the manifest half alone.
- **A permission gap should be visible in seconds, not after a full run.** The
  connection health probe (`InstallationInfo`) returns login + html_url but
  drops the `permissions` map GitHub already sends. Surfacing it would have
  named this before launch.
- Keep `draft_review=false` for unattended e2e; the gate otherwise parks the
  run at a human pause.

## 2026-07-17 — create-repo launch journey on CLOUD prod (run 019f7013)
- Status: validated
- Versions: bot 0.1.0 (+ manifest `repo:` block) · iterion edda80793 (cloud prod, ephemeral runner)
- Method: launched from the studio Launch form's NEW "Target repository →
  Create a new repository" mode (connection = PAT `devthejo`, owner
  SocialGouv, private) — the form created `SocialGouv/iterion-test-appy-e2e`
  on GitHub, then launched with `repo_url`+`connection_id`; the runner
  cloned the EMPTY repo (worktree:auto degraded in-place on the unborn
  HEAD, as designed). Vars: autonomous, draft_review=false, open_mr=true,
  max_passes=3; budget --max-cost-usd 8 --max-duration 35m. Prompt: a
  two-file static site (index.html + README) to keep the run tiny.
- Result: finished in 3m55s. Appy seeded main ("Initial commit", README),
  built the app on `iterion/improve/2997044` and opened PR #1 ("Add tiny
  static iterion e2e test site") with exactly the requested 2 files —
  clean, self-contained index.html.
- Value: proves the whole new journey — bot-declared repo need (`repo:
  {mode: optional, allow_create}`) → launch-form create → forge
  RepoCreator → empty-repo clone → build → publication.
- Findings / misses: Appy chose seed-main-then-branch+PR instead of the
  fresh-repo direct-push documented in forge-mr-create ("first push IS
  the publication"). Arguably BETTER (reviewable PR even on a fresh
  repo); consider aligning the skill's fresh-repo section with this
  observed shape rather than forcing direct push.
- Engine hardening (same campaign): RepoRequirement JSON wire-shape bug
  (yaml-only tags → studio saw `Mode`/`AllowCreate`, hiding the
  create/none options — fixed + wire-shape test); create-mode connection
  picker hid credential-fresh connections (repos-derived only — fixed by
  unioning listForgeConnections); `worktree: auto` hard-failed on
  unborn-HEAD repos (fixed: in-place degrade + test).
- Lessons for next run: keep budget caps on e2e tirs; a fresh PAT/App
  connection needs no provisioned repo to be a create target.

## 2026-07-16 — triple maiden run: autonomous NextJS+DSFR, interview→CLI, brownfield headless (runs 019f69a7 / 019f69a8 / 019f69b3)

- Status: validated (all three runs)
- Versions: bot 0.1.0 · iterion 34da65370 (worktree branch, pre-merge)
- Method: claude_code + opus-4-8 everywhere, sandbox-full:edge, host `~/.claude`
  OAuth mount, `--store-dir <repo>/.iterion` (operator studio visibility),
  `--merge-into none`. Three complementary scenarios:
  1. **autonomous** (019f69a7): empty non-git `/tmp` fixture,
     `app_prompt` = annuaire des administrations Next.js + DSFR
     (recherche, page détail, seed JSON, smoke test), `stack=nextjs-dsfr`.
  2. **interview** (019f69a8): empty fixture, `mode=interview`,
     near-empty prompt ("Un petit utilitaire CLI"), answers given via
     `iterion resume --answer message=…`; converged spec = `paristime`
     (heure Paris/UTC, stdlib only).
  3. **brownfield headless** (019f69b3): re-run of app-dev on the
     interview fixture (now a git repo), `draft_review=false`,
     `--max-cost-usd 15`, evolution prompt (option `--zone=<tz>`).
- Result: **all converged pass 1** (each request_changes round also
  reconverged in 1 pass).
  - autonomous: 6 commits pass 1 (`chore(scaffold)` → `feat(skeleton)`
    DSFR header/footer + smoke → `docs` README/ADRs → `feat(annuaire)`
    recherche + liste → détail + breadcrumb), verify green on real
    `npm run build` + 5 node tests (dont 404), review clean,
    draft_review pause with literal how_to_run; request_changes
    (« page /mentions-legales + lien footer ») → 1 more pass (2 commits:
    `feat(legal)` + `docs`) → reconverged → ship. $4.73 / 88.6k tokens /
    ~28 min wall (including both human pauses).
  - interview: 2 interviewer turns, **same claude session across turns**
    (sid 9a81d870… on both — the `_session_id` loop mapping works),
    `docs(spec): SPEC.md from operator interview` committed BEFORE any
    code, campaign shipped the CLI + tests, request_changes
    (`--format=iso`) → dedicated commit + 6 tests green → ship.
    $2.13, ~9 min of active time end-to-end.
  - brownfield: **no re-scaffold** (marker detection worked), 1 commit
    (`--zone` + tests), worktree ACTIVE this time (repo exists):
    commit banked on `iterion/run/atomic-bound-sonarsnoot-81d5`,
    fixture's checked-out `main` untouched (merge-into none), headless
    edge exercised (`draft_gate → mr_gate → done`, no human pause).
    $1.38, ~5 min.
- Value: the full product promise demonstrated in one afternoon — spec
  interview with real conversational memory, free-first-draft that
  builds+tests a real DSFR app, operator reframing loop, and safe
  evolution re-runs. The generated annuaire was **verified in a real
  browser (Playwright)**: DSFR banner + landmarks, labelled search
  (`?q=` shareable), filtered results (3 « mairie »), detail page
  (breadcrumb, adresse, horaires, contact `tel:`/`mailto:`, external
  link with « nouvelle fenêtre »), real 404 on unknown slug, legal
  notice page after the reframe. The paristime CLI was executed by
  hand (Paris/UTC/ISO/zones all correct).
- Findings / misses:
  - The campaign spontaneously produced ADRs (stack + data decisions)
    and a footer « Accessibilité : non conforme » declaration — the
    contract's ADR clause and the DSFR skill both landed.
  - Interview convergence is efficient but trusting: a terse operator
    answer ("on y va") converges immediately — fine for an expert,
    worth watching with less-specified briefs.
  - `verify_probe` forced verify.sh regeneration on every
    request_changes round (draft_loop re-entry keeps
    continuation_loop=0 → the iteration<=0 rule fired, ~$0.22/round).
    **Fixed same day**: app-dev's probe now decides staleness by a
    build-manifest fingerprint (sha256 of root manifests/lockfiles) —
    reuse while the toolchain is unchanged, regenerate the moment the
    scaffold or a dependency lands. Sibling bots keep the iteration
    rule (their single loop makes it correct).
  - Store layout note: run artifacts now live under `artifact_files/` +
    `turns/` (not `artifacts/<node>/<v>.json` as older docs say) —
    session-id checks must read `events.jsonl`.
- Engine hardening: none needed — no engine bug surfaced across the
  three runs (in-place degrade on non-git fixture, sandbox bind-mount
  writes, pause/resume cycles, loop bookkeeping
  `continuation_loop=0;draft_loop=1`, and worktree finalization all
  behaved per contract).
- Lessons for next run: (1) keep dogfood briefs small — the three-run
  matrix cost <$10 total; (2) for the studio-first UX test, launch via
  CLI against the workspace store and answer gates in the studio until
  the branch is merged (the main studio only discovers bots on its own
  tree); (3) gate friction: **addressed same day** — the studio's
  HumanPromptForm now renders one-click verdict buttons for `action`
  enum gates (Ship / Request changes / Hold for later), the same
  affordance the bool `approved` convention already had; bmady's menu
  gates benefit too.
