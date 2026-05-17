# Production Readiness Scorecard

**Scope:** workflow engine + editor + cloud + desktop + conductor +
sandbox surface.

**Effective:** end of Sprint-8 (2026-05-17).
**Branch:** `iterion/prod-readiness`.

**Verdict:** ✅ ready to ship a public v1 with the caveats called out
in the *Watch* column. The full-tree bug-review series (Sprint-3 →
Sprint-8) closed every P0 and every P1 the audits surfaced; the
remaining items below are P2/P3 ergonomic or scale-tier concerns
that don't gate a 1.0.

Use this document as the running checklist for every cut: a
component dropping a column from ✅ to ⚠️ or ❌ on a bump is a hard
review gate.

---

## Component scorecard

Legend: ✅ Ready · ⚠️ Watch · ❌ Block · — not applicable

| Component          | Build | Tests | Cov.   | Security | Obs.    | Docs | Release | Status |
|--------------------|:-----:|:-----:|:------:|:--------:|:-------:|:----:|:-------:|:-------|
| DSL (parser/IR)    | ✅    | ✅    | ✅ 80%+ | ✅       | n/a     | ✅   | ✅      | ✅      |
| Runtime engine     | ✅    | ✅    | ✅ 70%+ | ✅       | ✅      | ✅   | ✅      | ✅      |
| Backend (LLM)      | ✅    | ✅    | ✅ 70%+ | ✅       | ✅      | ✅   | ✅      | ✅      |
| Store / Mongo      | ✅    | ✅†   | ⚠️     | ✅       | ✅      | ✅   | ✅      | ✅      |
| Queue / NATS       | ✅    | ✅†   | ⚠️     | ✅       | ✅      | ✅   | ✅      | ✅      |
| Server (HTTP+WS)   | ✅    | ✅    | ⚠️ 22% | ✅       | ✅      | ✅   | ✅      | ✅      |
| Auth + Identity    | ✅    | ✅    | ✅     | ✅       | ✅      | ✅   | ✅      | ✅      |
| Conductor          | ✅    | ✅    | ✅     | ✅       | ✅      | ✅   | ✅      | ✅      |
| Sandbox            | ✅    | ✅    | ✅     | ✅       | ✅      | ✅   | ✅      | ✅      |
| Cloud (metrics/OTel)| ✅   | ✅    | ✅ 85%+ | ✅       | ✅      | ✅   | ✅      | ✅      |
| Runner             | ✅    | ⚠️*   | ⚠️ 18% | ✅       | ✅      | ✅   | ✅      | ⚠️     |
| Editor SPA         | ✅    | ✅    | ⚠️     | ✅       | n/a     | ✅   | ✅      | ✅      |
| Desktop (Wails)    | ✅    | ✅    | ⚠️     | ✅       | n/a     | ✅   | ⚠️‡    | ⚠️     |
| CLI                | ✅    | ✅    | ✅     | ✅       | n/a     | ✅   | ✅      | ✅      |
| Vendor (claw)      | ✅    | ✅    | n/a    | ✅       | n/a     | n/a  | ⚠️§    | ⚠️     |

† Conformance harness (`pkg/store/mongo`, `pkg/queue/nats`) runs in CI
against a real replica set / NATS, but unit-level coverage is light by
design — the contract is integration-tested.

\* Runner orchestrator (`processOne`, `executeRun`, the JetStream
consumer loop) needs full NATS + Mongo + executor fakes to be unit-
testable. Covered today by `cloud-e2e` + live e2e; documenting that
gap is the action item. Helpers (`metricsEmitter`, `normalizeModelLabel`,
`toFloat`) now have unit coverage.

‡ Desktop release pipeline produces signed releases on every tag, but
the macOS notarization + Windows EV cert + GPG key flow is **manual**
(see `docs/desktop-release-checklist.md`). Acceptable below ~1k
installs; revisit before broad consumer distribution.

§ Sprint-8 Phase-4 vendor bump pulled 4 P2 fixes via a transient
`replace github.com/SocialGouv/claw-code-go => ../../.works/claw-code-go`
directive. **Pre-merge action**: push the sibling branch upstream,
tag, then `go get @tag && go mod vendor` in this branch and drop the
replace.

---

## What Sprint-8 closed

Five previously-unaudited Go packages got a fresh-eyes review:
`pkg/git`, `pkg/identity`, `pkg/internal`, `pkg/log`, `pkg/runview` +
the naked `pkg/cloud` / `pkg/runner`. Findings rolled into the
hardening commits on this branch:

- **10 P1 fixes** — nil-writer logger panic, runview broker pointer
  aliasing, runview Resume `Source` plumbing, runner pre-lock
  `LoadRun` detached ctx, runner `TimeoutSec` finally wired,
  `LLMCostUSDTotal` counter finally populated, git `parseLog` tab
  handling, git path-flag injection, auth brute-force lockout,
  pending-password gate.
- **7 P2 fixes** — `mongoutil.IsIndexConflict` dedup,
  `pkg/log.Truncate` UTF-8 safety, log `LogBlock` trailing newline,
  runview eventstream subscribe ctx, cloud metrics `Default()`
  singleton, OTel sampler env honour, appinfo ldflags doc path.
- **Coverage backfill** — `pkg/cloud/metrics` 91.7%,
  `pkg/cloud/tracing` 85.1%, `pkg/runner` helpers 17.8% (orchestrator
  intentionally integration-tested).
- **Vendor bump + 4 P2 fixes in `claw-code-go`** — caching-scope beta
  flag, typed APIError on non-retryable returns, TTL-aware live
  registry refresh, default registry passed to `MaybeRefreshLive`.
- **Supply-chain hardening** — vendor-freshness CI gate, helm-lint
  job, govulncheck advisory, SBOM (syft, SPDX + CycloneDX),
  cosign keyless signing for binaries + server image + sandbox
  variants + Helm chart OCI artifact.
- **Repo hygiene** — `CODEOWNERS`, `dependabot.yml`, `SECURITY.md`,
  Mongo + blob backup/restore runbook.

---

## Pre-merge checklist (this branch)

- [ ] Push `.works/claw-code-go` branch `iterion-sprint-8-p2`
      upstream, tag (e.g. `v0.1.1`).
- [ ] Drop the transient `replace` from `go.mod`; bump via
      `go get github.com/SocialGouv/claw-code-go@<tag>`; re-run
      `go mod tidy && go mod vendor`; verify `git diff` shows no
      net vendor delta.
- [ ] Open the PR; let the new `vendor-check`, `helm-lint`,
      `govulncheck`, and `cloud-e2e` jobs gate it.
- [ ] On a clean post-merge: cut a `vX.Y.Z` tag and verify the
      `release.yml` SBOM + cosign signing artifacts attach to the
      GitHub release.

---

## Pre-cut checklist (every release)

- [ ] `task lint && task test && task test:e2e` clean locally.
- [ ] CI: all of `test`, `mongo-conformance`, `cloud-e2e`,
      `vendor-check`, `helm-lint`, `govulncheck` green.
- [ ] `task test:live:review` (the cheapest live target) clean on
      whatever provider you can prove credentials for.
- [ ] Chart version drift gate green (`task chart:check-version`).
- [ ] Skim `govulncheck` SARIF for new advisories; acknowledge or
      patch before tagging.
- [ ] If you bumped vendor: confirm no `replace` directives remain
      in `go.mod`.

---

## DEFER (out of scope here)

- **Wails desktop signing automation** — needs Apple notarization
  ($99/yr) + Windows EV cert ($300+/yr) + macOS CI runner. Manual
  signing acceptable until adoption justifies the cost. The release
  checklist is in [docs/desktop-release-checklist.md](docs/desktop-release-checklist.md).
- **CHANGELOG.md committed in-repo** — release-it + the
  conventional-changelog plug-in already publish GitHub release
  notes; a committed CHANGELOG.md would drift. The GitHub releases
  page is the single source.
- **Renovate** — Dependabot is sufficient for the five ecosystems
  iterion currently ships; switch cost is low if needed later.
- **Codecov threshold enforcement** — coverage signal is informative
  pre-1.0; threshold gates produce false PR failures during normal
  refactors. Re-evaluate post-1.0.
- **Race-detector CI job** — `task test:race` covers local runs
  cheaply; a dedicated CI lane adds marginal value for the runner
  budget.

---

## Watch list (post-1.0)

These don't gate v1 but are the natural next targets once the post-cut
review cadence kicks in.

1. **Runner orchestrator unit coverage** — `processOne` /
   `executeRun` / the JetStream consumer remain integration-only.
   Worth factoring a `runnerHarness` once we have appetite for the
   refactor.
2. **`pkg/store/mongo` unit coverage** — currently `(no test files)`
   in cached-test output because the conformance harness gates on
   `ITERION_TEST_MONGO_URI`. Adding pure-Go tests for the
   non-Mongo-touching helpers would broaden the signal.
3. **Editor `Settings/` vs `settings/` directories** — Sprint-7 P2
   flagged the parallel implementations (desktop keychain vs cloud
   BYOK). Naming consolidation; deliberate refactor.
4. **`api/runs.ts` validation of the `?store=` query parameter** —
   currently appended verbatim to API + WS URLs; Sprint-7 P2
   suggests a `[A-Za-z0-9_./-]` shape guard.
5. **TracerProvider sampler default** — Sprint-8 wired the env-driven
   sampler but kept `parent_based_always_on` as the default. Producing
   load-test numbers before flipping to a ratio-based default in
   production.
6. **`pkg/server/cloudpublisher` + `pkg/store/cloud` unit tests** —
   both currently report 0% coverage; integration paths exercise
   them, but a regression in the publisher's queue-position
   aggregation would only surface in cloud-e2e.

---

## Reference

- [.plans/bug-review-2026-05-16.md](.plans/bug-review-2026-05-16.md) —
  Sprint-5 findings.
- [.plans/bug-review-2026-05-17.md](.plans/bug-review-2026-05-17.md) —
  Sprint-6 findings.
- [.plans/bug-review-2026-05-17-sprint7.md](.plans/bug-review-2026-05-17-sprint7.md) —
  Sprint-7 findings (Go + editor TS first audit).
- [.plans/production-readiness-2026-05-17.md](.plans/production-readiness-2026-05-17.md) —
  this branch's execution plan.
- [SECURITY.md](SECURITY.md) — coordinated-disclosure channel.
- [docs/cloud-backup.md](docs/cloud-backup.md) — Mongo + blob backup
  runbook.
- [docs/cloud-troubleshooting.md](docs/cloud-troubleshooting.md) —
  symptom → diagnosis recipes.
- [docs/cloud-public-exposure-checklist.md](docs/cloud-public-exposure-checklist.md)
  — operator-facing pre-public hardening.
- [docs/desktop-release-checklist.md](docs/desktop-release-checklist.md)
  — desktop signing + notarization manual flow.
