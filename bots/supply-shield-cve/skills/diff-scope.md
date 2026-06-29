---
name: diff-scope
description: |
  How supply-shield scopes a run to the dependency versions a PR /
  push ADDED or UPGRADED. Read by the diff_scope tool node (lockfile
  glob list) and by enumerate_deps (how to read the diff). Keeps the
  set of inspected packages small + relevant — only what changed.
---

# diff-scope — scope a run to newly added/upgraded deps

supply-shield is a PR/push gate, not a full audit. The `diff_scope`
tool resolves the base..head range, finds the changed dependency
lockfiles, and emits their unified diff. `enumerate_deps` then reads
that diff and enumerates ONLY the `+` side resolved versions that are
new or upgraded — never the unchanged dependency set.

## Range resolution (diff_scope)

- `scope_mode = full` → no diffing; the enumerator walks the whole
  workspace (node_modules / .venv / vendor + lockfiles), exactly like
  sec-audit-deps. Use for a one-off whole-tree pass.
- `scope_mode = diff` (default):
  - `base_ref` empty → auto-detected: `git merge-base HEAD origin/HEAD`,
    else `origin/main`, else `origin/master`, else `HEAD~1`.
  - `head_ref` defaults to `HEAD`.
  - changed files = `git diff --name-only base...head`, filtered to the
    lockfile globs below.
  - the unified diff of just those lockfiles (bounded to ~20 KB) is
    handed to the enumerator.

## Lockfile globs (machine-readable — consumed by diff_scope)

`diff_scope` reads this block to decide which changed files are
dependency lockfiles. Globs are matched against the file BASENAME with
`fnmatch`. Add an ecosystem's lockfile here — no DSL change.

<!-- iterion:lockfiles
["package-lock.json","npm-shrinkwrap.json","yarn.lock","pnpm-lock.yaml","bun.lockb","go.mod","go.sum","requirements*.txt","poetry.lock","uv.lock","Pipfile.lock","Cargo.lock","Gemfile.lock","composer.lock"]
-->

## Reading the diff (enumerate_deps)

For each changed lockfile, parse the `+` (new) side and the `-` (old)
side; a package is **newly added/upgraded** when its resolved
`name@version` appears on the `+` side and not (at that version) on the
`-` side. Examples:

- `package-lock.json` / `pnpm-lock.yaml` / `yarn.lock`: compare the
  resolved `name@version` + `integrity` entries.
- `go.sum`: a new `module version h1:hash` line is a new pin.
- `requirements.txt` / `poetry.lock` / `uv.lock`: a new or bumped
  pinned version.

When a lockfile is added wholesale (only `+` lines), every resolved
version in it is new. When `node_modules` / `.venv` / `vendor` is also
present, prefer the installed artifact for the checksum; otherwise use
the lockfile integrity / hash field.

## Why diff-scope

A full audit on every PR re-inspects hundreds of unchanged, already-
cached packages — slow and noisy. Diff-scoping plus the shared
`[[package-cache]]` means a PR only pays for the genuinely new versions,
and even those are skipped if another run already cleared them.

## See also

- `[[package-cache]]` — the shared store that dedups across runs.
- `[[supply-shield-cve]]` — the orchestrating playbook.
- `[[forge-report]]` — how the result is posted back on the PR.
