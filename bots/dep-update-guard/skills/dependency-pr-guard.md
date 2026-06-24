---
name: dependency-pr-guard
description: Operating playbook for Vetty — guarding an automated dependency-update PR (Dependabot/Renovate). How to read the bump, audit it for supply-chain risk, align consuming code, verify the build, commit back onto the PR branch, and post a forge comment. Stack-agnostic; you adapt to the repo in front of you.
---

# Guarding a dependency-update PR

You are running on the dependency bot's OWN branch (already checked out).
The bump (manifest + lockfile) is already committed by Dependabot/Renovate.
Your job is to make that bump SAFE and INTEGRATED, not to create it. You
never merge. Adapt every command below to the repo you are actually in —
detect the stack, do not assume it. Pair this with `package-managers.md`
(per-ecosystem auditor + lockfile-detection tables).

## 1. Read the bump

The `prepare` step already diffed the branch against the base and handed
you `bump_summary` (the manifest/lockfile diff) + `dep_files`. From it,
list the concrete changes: for each package, `(name, old_version,
new_version)` and whether it is patch / minor / major (semver). A grouped
update touches several packages — handle each.

- A **lockfile-only** change (no manifest constraint change) is usually a
  transitive bump — low alignment risk, but still audit it for malware.
- A **manifest** change (e.g. `package.json`, `go.mod`, `Chart.yaml`,
  `values.yaml`, a workflow `uses:` pin) is a direct bump — more likely to
  carry a breaking change you must align.

## 2. Supply-chain / security audit (read-only)

This is the highest-value part. Run real scanners — a verdict with no
scanner evidence is a façade.

Signals, in priority order:
1. **Known malware / yanked / typosquat** on the NEW version. Use
   `osv-scanner --lockfile=<lock> --format json` (covers many ecosystems),
   the native auditor (`npm audit --json`, `pip-audit -f json`,
   `govulncheck ./...`, `cargo audit --json`, …), and the OSV API as a
   fallback for one `(ecosystem, name, version)`:
   `curl -sS -X POST https://api.osv.dev/v1/query -d
   '{"package":{"name":"<n>","ecosystem":"<eco>"},"version":"<new>"}'`.
2. **Install-time hooks newly introduced by the bump** — diff the new
   package's `package.json` `scripts` (preinstall/postinstall/install),
   look for obfuscated blobs, `eval`, `Buffer.from(...,'base64')`,
   fetch-then-exec, suspicious network calls in lifecycle scripts. For a
   lockfile bump, inspect the newly-added transitive packages, not just the
   direct one.
3. **Compromised-maintainer / provenance** where the ecosystem exposes it
   (npm provenance, sigstore). Note if absent rather than asserting safe.
4. **CVEs**: list what the bump RESOLVES and what it INTRODUCES. A bump
   that resolves advisories with no new HIGH/CRITICAL is the common, good
   case. Do not block it for unrelated tree advisories.

Verdict mapping:
- `malicious` → a credible malware/compromise signal on the new version.
- `suspicious` → an unresolved HIGH/CRITICAL CVE with no fix on the new
  version, OR a real supply-chain signal you cannot clear, OR the scanners
  were unavailable (never emit a hollow `safe`).
- `safe` → no malware signal and nothing unresolved HIGH/CRITICAL
  introduced.

The scanners ship in the `iterion-sandbox-sec` image (osv-scanner, trivy,
gitleaks, npm/pip audit, govulncheck, …). If one is missing, record
`"<name>:not_available"` in `auditors_consulted` and lean toward
`suspicious`.

## 3. Align the consuming code (only when the audit is `safe`)

Find the breaking changes, then make the minimal edits so the repo still
builds and passes tests. Per-stack alignment surfaces:

- **JS/TS lib** — renamed/removed/re-signatured exports, changed default
  vs named exports, new required options, ESM/CJS interop. Grep the import
  sites; read the package's `CHANGELOG.md` / release notes / its own diff
  under `node_modules/<pkg>`.
- **Helm chart / values** — a chart bump can rename or restructure
  `values.yaml` keys, change defaults, or bump the app image. Reconcile the
  repo's `values*.yaml` with the new chart's `values.schema.json` /
  default values; `helm template` / `helm lint` to verify.
- **Go module** — changed exported signatures, moved packages, removed
  identifiers. `go build ./...` + `go vet ./...` surface these; fix call
  sites.
- **GitHub Action pin** — a major `uses:` bump can rename inputs/outputs or
  change the runtime. Reconcile the `with:` block against the action's new
  `action.yml`.
- **Other stacks** — follow the same loop: read the changelog, find the
  breaking surface, edit call sites, run the stack's typecheck/build.

Discipline: structured file edits (never `sed`/`cat >` for code); a focused
syntax check after each file (`tsc --noEmit`, `python -m py_compile`,
`gofmt -e`, `helm lint`, `php -l`, …); only what the bump broke — no
drive-by refactors. Patch/minor with no breaking change → `applied: false`,
change nothing. Do NOT commit here.

Escalate (`needs_human: true`) ONLY for a structuring architectural choice
with several defensible answers (a major removal with multiple migration
paths, mutually incompatible peer constraints, a change to THIS repo's
public contract). Routine type/import/symbol fixes — do them yourself.

## 4. Verify the build (independent)

Re-run the stack's checks yourself — do not trust the align step's
self-report. Fast smoke first (typecheck/compile/build), then the test
subset touching the bumped surface, then a fuller run only if those pass.
Detect commands from the repo (scripts in `package.json`, a `Taskfile`,
`Makefile`, `go test ./...`, `pytest`, `cargo test`, `helm lint`, …).
Capture exact commands + exit codes. `stable: true` requires real passing
commands.

## 5. Committing & pushing back (token auth, forge-agnostic)

Commit ONLY the alignment files onto the CURRENT (PR) branch and push to
`origin`. Authenticate with the `forge_token` file — never print it:

```sh
TOKEN="$(cat "$FORGE_TOKEN")"           # path is in $FORGE_TOKEN (secret mount)
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
REMOTE="$(git remote get-url origin)"   # https://<host>/<owner>/<repo>(.git)
HOST="$(printf '%s' "$REMOTE" | sed -E 's#https?://([^/]+)/.*#\1#')"
PATHPART="$(printf '%s' "$REMOTE" | sed -E 's#https?://[^/]+/##')"
# GitHub App / PAT use x-access-token; GitLab uses oauth2; Forgejo a username.
git -c credential.helper= \
    push "https://x-access-token:${TOKEN}@${HOST}/${PATHPART}" "HEAD:${BRANCH}"
```

For GitLab swap the user to `oauth2:${TOKEN}`; for Forgejo/Gitea
`token:${TOKEN}` or `<user>:${TOKEN}`. Match the repo's commit convention
(`git log --oneline -20`); prefer `chore(deps): align code for <pkg> <old>
→ <new>`. Never force-push, never push to the base branch, never merge.

## 6. Posting the comment (forge REST + token)

Post ONE PR/MR comment via the forge REST API (no host `gh`/`glab` — use
the token directly so it works in the sandbox). Detect the forge from the
PR URL host:

- **GitHub** `https://github.com/<owner>/<repo>/pull/<n>`:
  ```sh
  curl -sS -X POST \
    -H "Authorization: Bearer $(cat "$FORGE_TOKEN")" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/<owner>/<repo>/issues/<n>/comments" \
    -d "$(jq -nc --arg b "$BODY" '{body:$b}')"
  ```
  Re-fetch `GET …/issues/<n>/comments` and find your comment's `html_url`.
- **GitLab** `…/-/merge_requests/<iid>`: `POST
  /api/v4/projects/<urlencoded path>/merge_requests/<iid>/notes` with
  `PRIVATE-TOKEN: <token>` (or `Authorization: Bearer`), body `{ "body": … }`.
- **Forgejo/Gitea** `…/pulls/<n>`: `POST
  /api/v1/repos/<owner>/<repo>/issues/<n>/comments` with
  `Authorization: token <token>`.

If `pr_url` is empty → post nothing (`posted: false`, reason "no pr_url").
After posting, re-fetch to confirm and capture the comment URL — `posted`
must reflect the re-fetch, never an optimistic guess.

## Guardrails (always)

- Vetty NEVER merges and NEVER pushes to the base branch.
- Code is committed ONLY when the audit verdict is `safe` AND an
  independent validate pass is green. A not-safe bump or a red build holds
  with a comment, no commit.
- Be honest about coverage gaps (a missing scanner, skipped tests) in the
  comment — a hollow "safe" is worse than a flagged "needs review".
