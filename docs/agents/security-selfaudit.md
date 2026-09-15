# Security self-audit — running iterion's own security bots on iterion

How this repo audits itself: the scanner toolchain, the sec sandbox image, the
recurring schedule, and the standing baseline of what has already been
resolved. The bots themselves are documented in
[../security-bots.md](../security-bots.md); this page is the self-host case.

## Security

Iterion self-audits with its own catalog bots, `sec-audit-source`
(SAST) and `sec-audit-deps` (SCA), pointed at this repo. Findings land
on the native board with the label **`source:sec-audit-self`**;
critical/high are triaged into roadmap items, medium/low stay in the
inbox.

**Scanner toolchain.** The scanner binaries (semgrep, gosec,
govulncheck, bandit, pip-audit, trivy, gitleaks) ship in the
**`iterion-sandbox-sec`** image (`sandbox/sec/Dockerfile`, layered on
`-full`), which both bots pin via `sandbox.image`. A bare host and the
slim/full images have none of these tools, so running the bots without
the sec image produces a zero-finding façade — now caught, not silent:
`sec-audit-source`'s deterministic `scan_health` gate hard-fails the run
when the always-on generic scanners (gitleaks/trivy/semgrep-auto)
produced no output, and banners partial coverage gaps in the report (see
[sec_audit_scan_health_test.go](../../e2e/sec_audit_scan_health_test.go)). CI publishes it
in two halves: the tool-only `iterion-sandbox-sec-base` builds in
[.github/workflows/sandbox-images.yml](../../.github/workflows/sandbox-images.yml)
(only when `sandbox/**` changes), and the published
`iterion-sandbox-sec:edge` is finalized — current iterion binary stamped
onto that base — on every push to `main` by
[.github/workflows/image.yml](../../.github/workflows/image.yml) via
[_finalize.yml](../../.github/workflows/_finalize.yml) (and at `:vX.Y.Z` on
release tags). For a local-only loop, build it yourself and `docker tag`
it to `ghcr.io/socialgouv/iterion-sandbox-sec:edge`.

**Recurring audit.** The weekly schedule (sec-audit-source Mon 02:00
UTC, sec-audit-deps Mon 03:00 UTC) is wired through
[`iterion schedule`](../scheduling.md) — a host-crontab integration
that needs **no resident daemon** (the host's own cron is the trigger).
Register and install it with:

```sh
iterion schedule add sec-audit-source-weekly \
  --cron "0 2 * * 1" --bot bots/sec-audit-source/main.bot --workdir "$PWD"
iterion schedule add sec-audit-deps-weekly \
  --cron "0 3 * * 1" --bot bots/sec-audit-deps/main.bot --workdir "$PWD"
iterion schedule install            # splices a managed block into `crontab`, CRON_TZ=UTC
```

Note: `sec-audit-source` (SAST) is production-ready (cap_findings +
scan_health hardened). `sec-audit-deps` (SCA) now has a **real CVE floor**:
`run_generic_heuristics` runs `trivy fs --scanners vuln` over the workspace,
matching every pinned version in go.mod / package-lock.json / requirements.txt
/ Cargo.lock etc. against the OSV/GHSA/NVD DB **from a bare checkout** (no
`npm/pip install` needed) — validated producing 10 corroborated CVEs on a
`lodash@4.17.4` lockfile, zero false positives. The per-ecosystem npm-audit/
pip-audit heuristics still need an installed tree, and the code-pattern /
typosquat-corpus malware signals remain pending (native:3a81df64), so a run
still banners partial coverage — but it is no longer a 0-finding scaffold.
(In a sandboxed run the board tools ride the HTTP transport —
`/api/v1/mcp/board` with an ephemeral run token; known gap: on Linux
docker the in-container endpoint can be unreachable (native:e6cd506e),
in which case findings land in the markdown report instead of the board.)

Each cron line routes through `iterion schedule run <name>`, which
re-reads `~/.iterion/schedules.yaml` so the manifest stays authoritative;
logs land in `~/.iterion/logs/schedule-<name>.log`. Of the three original
blockers, the context-overflow ones are fixed —
`sec-audit-source`'s `detect_tech`/`triage` overflow is bounded by the
deterministic `cap_findings` node (see
[sec_audit_cap_findings_test.go](../../e2e/sec_audit_cap_findings_test.go)).
The remaining gate before flipping the schedule on for real is **(2) the
sec image published in CI** (the sandbox-images.yml `base-sec` job + the
per-push finalize above); until that first push lands, install the
schedule but `docker tag` the locally built `iterion-sandbox-sec:edge`
so the scanned runs find their tools.
For a one-time audit by hand, a direct scanner pass in the sec image is
reliable —
`docker run --rm -v "$PWD":/src:ro -w /src
ghcr.io/socialgouv/iterion-sandbox-sec:edge gosec -severity=high
-confidence=high -exclude-dir=vendor -exclude-dir=.iterion ./...`.

The 2026-05-31 self-audit surfaced 6 high-severity gosec taint findings
(SSRF in `pkg/server/runs_preview.go`, path-traversal in
`pkg/server/runs_files.go` + a few internal paths); **all were resolved in
`c9e18195`** — the strict-mode SSRF gate (public-unicast
pinning, metadata/loopback/link-local blocks, DNS-rebinding-proof, no
redirect-follow), since extracted to [`pkg/secure/httpdial`](../../pkg/secure/httpdial/httpdial.go)'s
`ResolvePublicHost` (the single source of truth, now also backing completion
webhooks and OIDC SSO), and `safePathWithin` symlink-aware containment for run-file
read/write, with regression tests in `runs_preview_test.go` /
`runs_files_test.go`. New `source:sec-audit-self` findings land on the board;
verify against the code before re-surfacing a finding as open (the prose
above is the standing baseline, not an open-work list).

