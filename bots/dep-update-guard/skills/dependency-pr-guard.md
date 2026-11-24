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
   fallback for one `(ecosystem, name, version)` — query it for `<old>`
   and for `<new>`, with the package's id in that ecosystem (the Go
   toolchain is two ids, `stdlib` and `toolchain`) and the bare resolved
   version (`1.24.3`, never `go1.24.3` or `^1.2.3`). OSV answers an
   unknown id or an unparseable version without an error: confirm the id
   is known (a query without a version returns its records) before
   reading an empty answer as clean:
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
4. **CVEs** (vulnerability advisories — a malware, typosquat or yanked
   advisory, or any record stating the package or version itself ships
   malicious code, is signal 1 and `malicious` on the new version,
   whatever the old pin carries; a flaw through which an attacker
   injects or runs malicious code is a vulnerability): sort what you find into RESOLVED (old pin affected,
   new version not), INTRODUCED (new version affected, old pin not) and
   REMAINING (both affected). A package the bump adds has no old pin: its
   advisories are INTRODUCED; one it removes has only RESOLVED ones. Sort
   from the advisory record (OSV, GHSA, NVD, by id) checked against BOTH
   pins — scan the old lockfile too (`git show "$(git merge-base <base>
   HEAD)":<lockfile>` into a scratch directory, under its own name), or
   query OSV for each pin;
   a scanner line that prints only a fixed version is not an unpublished
   range. Which pins an advisory affects comes from its record, never
   from release notes, a changelog or the PR body. With an unpublished
   affected range, an advisory fixed only after both pins is REMAINING
   unless evidence puts the old pin outside it. A bump that resolves
   advisories with no new HIGH/CRITICAL is the common, good case. Do not
   block it for unrelated tree advisories, nor for a REMAINING one:
   holding keeps the old pin, which carries it too. Report a remaining
   advisory with the release that fixes it and whether the bump's own
   source already ships that release. An INTRODUCED advisory counts at
   the highest severity any of its records gives (GHSA, NVD, the CNA),
   and as HIGH when no record scores it (a Go vulndb `GO-` record carries
   no severity; NVD may not have scored the CVE yet).

Verdict mapping:
- `malicious` → a credible malware/compromise signal on the new version.
- `suspicious` → a HIGH/CRITICAL CVE the bump INTRODUCES, OR a real
  supply-chain signal you cannot clear, OR no auditor could read the
  changed packages (never emit a hollow `safe`).
- `safe` → no malware signal and no HIGH/CRITICAL the bump introduces;
  REMAINING advisories are reported, never held.

The scanners ship in the `iterion-sandbox-sec` image (osv-scanner, trivy,
gitleaks, npm/pip audit, govulncheck, …). A scanner is missing only once
you ran it with Bash and it failed — its binary is absent or cannot run:
record `"<name>:not_available"` in `auditors_consulted` and quote the
command and its error. A scanner that reports advisories has not failed,
whatever its exit code, for the lockfiles its report covers: read its
report; a lockfile it errored on was not read. One whose output is an
error (no lockfile, registry unreachable) read nothing: it is
unavailable. One
missing scanner is a coverage note; `suspicious` only when no auditor
could read the changed packages.

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

**A lockfile the bump left stale IS part of the alignment.** Some bots edit
the manifest without regenerating the lockfile, and CI installs frozen
(`pnpm install --frozen-lockfile`, `npm ci`, `yarn --immutable`,
`go mod tidy` + a vendor-check, `poetry check --lock`, `bundle install
--frozen`). The install then fails on a mismatch — *"specifiers in the
lockfile don't match specifiers in package.json"* — before a single line of
code is compiled. That is not the bump breaking the repo, it is the bump
being incomplete, and regenerating the lockfile is a mechanical fix squarely
in scope: run the ecosystem's own resolve step (see `[[package-managers]]`
for the per-manager command), commit the regenerated lockfile with the rest
of the alignment, and say so in `breaking_summary`. Do NOT reach for
`--no-frozen-lockfile` to make the error go away: that hides the drift from
CI instead of fixing it, and the merged branch stays unbuildable.

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
`origin`. Authenticate with the `forge_token` file — never print it. The
token reaches git through its ENVIRONMENT, as an `Authorization` header:
never in a URL and never on a command line, where every process in the
sandbox — the bumped package's install scripts included — can read it:

```sh
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
REMOTE="$(git remote get-url origin)"   # https://<host>/<owner>/<repo>(.git)
HOST="$(printf '%s' "$REMOTE" | sed -E 's#https?://([^/@]+@)?([^/]+)/.*#\2#')"
PATHPART="$(printf '%s' "$REMOTE" | sed -E 's#https?://[^/]+/##')"
# GitHub App / PAT use x-access-token; GitLab uses oauth2; Forgejo token.
# The token file's PATH is in $FORGE_TOKEN (secret mount); printf is a shell
# builtin, so the token itself never becomes a process argument.
N="${GIT_CONFIG_COUNT:-0}"
export "GIT_CONFIG_KEY_${N}=http.https://${HOST}/.extraHeader"
export "GIT_CONFIG_VALUE_${N}=Authorization: Basic $(printf 'x-access-token:%s' "$(cat "$FORGE_TOKEN")" | base64 | tr -d '\n')"
export GIT_CONFIG_COUNT="$((N + 1))"
git -c credential.helper= push "https://${HOST}/${PATHPART}" "HEAD:${BRANCH}"
```

For GitLab swap the user to `oauth2`; for Forgejo/Gitea `token` or your
username — always inside the header, never in the URL. Match the repo's
commit convention (`git log --oneline -20`); prefer `chore(deps): align
code for <pkg> <old> → <new>`. Never force-push, never push to the base
branch, never merge.

## 6. Posting the comment (forge REST + token)

Post ONE PR comment via the forge REST API (no host `gh`/`glab` — use
the token directly so it works in the sandbox). The token travels as a
header curl reads from its STDIN (`-H @-`), never on its command line,
where every process in the sandbox — the bumped package's install scripts
included — can read it; `printf` is a shell builtin, so the token never
becomes a process argument. Detect the forge from the PR URL host:

- **GitHub** `https://github.com/<owner>/<repo>/pull/<n>`:
  ```sh
  printf 'Authorization: Bearer %s\n' "$(cat "$FORGE_TOKEN")" |
    curl -sS -X POST -H @- \
      -H "Accept: application/vnd.github+json" \
      "https://api.github.com/repos/<owner>/<repo>/issues/<n>/comments" \
      -d "$(jq -nc --arg b "$BODY" '{body:$b}')"
  ```
  Re-fetch `GET …/issues/<n>/comments` the same way (the header on stdin)
  and find your comment's `html_url`.
- **GitLab** `…/-/merge_requests/<iid>`: `POST
  /api/v4/projects/<urlencoded path>/merge_requests/<iid>/notes` with
  `PRIVATE-TOKEN: <token>` (or `Authorization: Bearer`) on curl's stdin as
  above, body `{ "body": … }`.
  **Quick-action guard (mandatory):** GitLab runs `/merge`, `/approve`, …
  written at the start of a line of a note, with the posting account's
  rights — and the comment quotes an untrusted changelog. Escape first, the
  same rewrite the server-side guard applies (#2413, #2422; the
  `[^[:print:]]` branch covers what `[[:alpha:]]` misses under `LC_ALL=C`,
  e.g. `/ſpend 1h`; `[:print:]`, not `[:ascii:]` — GNU sed rejects
  `[:ascii:]`, and `[^[:print:]]` catches the same bytes on GNU and BSD):
  ```sh
  BODY="$(printf '%s' "$BODY" | sed -E 's#^([[:blank:]]*)/([[:alpha:]]|[^[:print:]])#\1\\/\2#')"
  ```
- **Forgejo/Gitea** `…/pulls/<n>`: `POST
  /api/v1/repos/<owner>/<repo>/issues/<n>/comments` with
  `Authorization: token <token>` on curl's stdin as above.

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
