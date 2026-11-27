# ADR-177 — Security posture is a dedicated bot that consumes the forge's alerts; Vetty keeps the per-PR lane

- **Status**: Proposed — the operator chose the direction on 2026-11-26. The bot itself is not built yet ([#3770](https://github.com/SocialGouv/iterion/issues/3770)).
- **Date**: 2026-11-26
- **Deciders**: jo (the decision), Claude (the measurements and the study)
- **Relates to**: [ADR-124](124-gating-bot-merge-gestures.md) (gating bots and their merge gestures) · [ADR-169](169-a-run-reads-no-environment-and-the-catalogue-names-roles-not-vendors.md) (routing is a database override, never an environment variable)
- **Serves**: [#3770](https://github.com/SocialGouv/iterion/issues/3770) · epic [#3526](https://github.com/SocialGouv/iterion/issues/3526) · follows [#3602](https://github.com/SocialGouv/iterion/issues/3602), [#3730](https://github.com/SocialGouv/iterion/issues/3730) · siblings [#3767](https://github.com/SocialGouv/iterion/issues/3767), [#3768](https://github.com/SocialGouv/iterion/issues/3768), [#3769](https://github.com/SocialGouv/iterion/issues/3769), [#3771](https://github.com/SocialGouv/iterion/issues/3771)

## Context

Between 2026-11-24 and 2026-11-26 an interactive session brought this repository's security alerts back in line by hand:
- govulncheck: 37 reachable vulnerabilities → 0;
- Trivy: 1135 open alerts → about 60, each remaining one accepted with a reason;
- Dependabot: 71 alerts → 6.

The causes it found were structural, not incidents:

1. **Detection without a consumer.** CI already ran govulncheck and Trivy. The govulncheck SARIF upload had been refused since it was added, and `continue-on-error` hid the refusal. Nobody read the results.
2. **Pins nobody moved.**
   - The golang builder digest was frozen at 1.27.0, so the production image shipped stdlib CVEs fixed months earlier. The cause showed up later. Renovate's "docker base images" group has been listed as *PR Edited (Blocked)* on the dependency dashboard since 2026-08-24: its PR merged on a head that was no longer Renovate's, and the repository does not delete merged branches. So Renovate opened no base-image PR for three months.
   - Tool versions in Dockerfile `ARG`s carried no Renovate annotation, and the scanner image's Dockerfile was not even in Renovate's file list ([#3771](https://github.com/SocialGouv/iterion/issues/3771)).
3. **Dependency PRs nobody judged.** Vetty was bound to this repository, but its agents fell back to an expired credential: 25 runs since 2026-10-12, 0 verdicts. Each paused run held the required check PENDING ([#3767](https://github.com/SocialGouv/iterion/issues/3767), [#3768](https://github.com/SocialGouv/iterion/issues/3768)).
4. **Noise, and decisions only a person makes.** Most of the alert volume came from one unmaintained tool, from deprecated backends and from fixtures. Clearing it took product decisions: drop k9s, do not migrate the deprecated pi backend, decline vitepress v8.

The catalogue already holds seven security bots. Measured on iterion.cloud on 2026-11-26, none of them consumes a repository's own alert backlog:

| bot | lane | what it does | real use |
|---|---|---|---|
| Vetty (`dep-update-guard`) | per PR | audits a dependency bump, aligns code, builds, publishes the gate | 353 runs on buildkit-operator, 28 of 29 PRs ended green |
| Seki (`sec-audit-source`) | on demand | SAST + secrets + `trivy fs`, LLM triage, board issues | 38 manual runs, September |
| Depsy (`sec-audit-deps`) | on demand | malware heuristics + CVE floor over enumerated deps | 0 cloud runs since June |
| Vulny (`supply-shield-cve`) | per PR | CVEs a diff introduces | — |
| Shieldy (`supply-shield`) | per PR | malware a diff introduces | — |
| Senti (`vuln-watch`) | hourly, org-wide | exploited advisories (KEV, EPSS) matched to an inventory, alerts to chat, **zero LLM by design** | — |
| Renovacy (`secured-renovacy`) | on demand | opens upgrades proactively | 0 cloud runs since June |

Seki and Depsy re-scan the tree instead of reading what CI already published. Their weekly schedules were host-crontab lines that never ran.

## Decision

### 1. Two lanes with two owners

- **Per PR: Vetty.** It judges each dependency PR, aligns the consuming code and publishes the gate. This lane already works, and it is not extended to periodic work. Its continuity, gate and arming machinery is specific to a pull request.
- **Periodic posture: a new, dedicated bot.** Working name `sec-posture`; the operator names the persona (the y/i/ie rule; "Posty" and "Patchy" are free). It owns the backlog: what CI and the forge already report about the default branch.

### 2. It consumes, it does not re-scan

Its input is the forge's alert APIs: code scanning (where govulncheck and Trivy SARIF land) and Dependabot alerts. It reads them deterministically, in tool nodes. Reading goes through `pkg/forge`, so GitLab's vulnerability report is the next variant, not an engine branch. Senti's `poll_dependabot` is the precedent to share. The bot runs no scanner of its own. Scanning stays CI's job, and a dead channel is reported as a posture fact (§4), never filled in by a second scan.

### 3. One LLM pass over the delta

A deterministic step diffs the alerts against the previous run's state. One triage agent classifies only the new and changed alerts:
- reachable or not;
- fix published or not;
- deprecated component;
- fixture or false positive;
- major upgrade required.

Unchanged alerts keep their classification.

### 4. Output: proposals first, gestures after calibration

- **Dismissals** are proposed with their reason. The bot ships in `dry_run` mode, as Warden does: dismissals are listed for the operator, and a per-repository pin turns on `enforce`, where the bot dismisses through the API.
- **Remediation lots** are board tickets, grouped by fix: one bump that closes N alerts, one image rebuild. A later version opens the PR. Vetty judges that PR like any other, which closes the bot → bot loop.
- **Posture metrics** go on every run: counts by tool and severity, fix-available counts, the age of the newest analysis per scanning tool, and the update groups Renovate's dependency dashboard reports as blocked or erroring. A tool that stopped publishing, or a group that stopped moving, is reported loud.
- **Product decisions** are escalated, never taken: retiring a component, declining a major, accepting a residual risk.

### 5. A cloud schedule, not a host crontab

The bot runs from the server's schedules, with a bounded per-run budget, and its state is durable on the cloud side. A run is idempotent: re-running it over unchanged alerts proposes nothing new.

## Options considered

- **Evolve Seki.** Seki has the right skeleton: LLM triage, three-voter revalidation, `scan_health`, remediation, board. But its mandate is the source code it scans, and its three opus voters are sized for tens of findings, not thousands of alerts. Rejected: it would blur a bot the operator rates as promising.
- **Extend Vetty with a sweep mode.** Rejected: every one of its mechanisms is keyed on one pull request, and the bundle is already the largest in the catalogue.
- **Evolve Senti.** It already polls Dependabot alerts on a schedule. Rejected: its zero-LLM invariant is deliberate ("a run can neither spend a token nor show a project name to a model"), and triage needs a model.
- **A deterministic weekly job, and the bot later.** Rejected as the endpoint, because triage and remediation, which were the bulk of the manual work, would stay manual. It survives as §4's metrics, which are deterministic.

## Consequences

- The security catalogue grows to eight bots, while four of them have no recent cloud use. Consolidating Depsy, Shieldy, Vulny and Renovacy is out of scope here and belongs to the catalogue pruning.
- Vetty must be healthy on every repository the posture bot hands PRs to. [#3767](https://github.com/SocialGouv/iterion/issues/3767) and [#3768](https://github.com/SocialGouv/iterion/issues/3768) come first.
- Renovate stays the source of routine bump PRs ([#3771](https://github.com/SocialGouv/iterion/issues/3771): OSV security PRs, family grouping, a lint against unmanaged pins). The posture bot covers only what Renovate does not.
- A false-positive dismissal hides an alert. That is why dismissal starts as a proposal, and why every dismissal carries its reason.

## Open questions

- The persona name.
- The cadence (weekly by default?) and the per-run budget.
- Whether remediation PRs ship in v1 or after the ticket-only calibration.
- The required write scopes (`security_events: write` for dismissals), and their grant in `enforce` mode only.
