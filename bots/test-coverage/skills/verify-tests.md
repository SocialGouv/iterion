---
name: verify-tests
description: Detect and run a repository's OWN test suite (and coverage tool, if any) so a deterministic gate can confirm the new tests actually pass before Testy commits. Stack-agnostic — read this when adding tests or asked to verify them.
---

# verify-tests

Your job: make the repository's tests — **including the ones you just
added** — pass **using the repo's own tooling**, and leave a re-runnable
script the deterministic gate executes to confirm it.

This is the floor of the whole bot. The cross-family reviewers judge
whether your tests are *meaningful*; this gate proves they at least
*compile and pass* and that you *actually added* test code. A coverage
bot whose tests don't run is a façade by definition.

## 1. Detect the repo's test command

Do **not** assume a language. Look at the markers actually present and
prefer the project's own wrapper — it encodes the correct flags and
toolchain:

- **Task runner / Makefile** — `Taskfile.yml` → `task test`; `Makefile` →
  `make test`; `Justfile` → `just test`; `package.json` scripts →
  `<pkgmgr> test`. Almost always the right answer when present.
- **Pinned toolchain — honour it or the suite fails on a version
  mismatch.** `devbox.json` → prefer `devbox run -- …`.
  `.tool-versions` (asdf/mise), `.nvmrc`, `flake.nix`, `mise.toml` →
  activate accordingly. For Go, if `go.mod`'s `go` directive is newer than
  the installed toolchain, use the wrapper (`devbox run -- go …`) or
  `GOTOOLCHAIN=auto go …` rather than failing on "requires go >= X".
  **Sandbox/CI caveat:** in a locked-down container the wrapper's own cache
  dir may be unwritable — e.g. `devbox` fails with
  `mkdir: cannot create directory '~/.cache/devbox': Permission denied`. If
  a wrapper errors on its cache, retry once with `XDG_CACHE_HOME=/tmp/cache`
  (or `HOME=/tmp`), and if it still fails, fall back to a toolchain the
  environment already provides directly (e.g. plain `go test ./pkg/...`
  when Go is on PATH). A direct-toolchain verify
  script that genuinely runs the tests is better than a wrapper that can't
  start. Put whatever finally worked into `.test_coverage.verify.sh`.
- **Language defaults (only when there is no wrapper)** — pick the runner
  the repo already uses (look at existing tests + CI). See [[test-types]]
  for per-stack commands. When in doubt, read `.github/workflows/*` (or
  other CI config): CI is the source of truth for how this repo tests
  itself.

**Scope the run to keep the loop fast.** Prefer running the tests for the
package/area you touched (e.g. the repo's per-package test invocation)
plus a whole-module compile, rather than the entire slow suite, *unless*
the repo only has one suite. Skip slow live/e2e suites during the inner
loop unless e2e is exactly what was requested.

## 2. Measure coverage (only if the repo already supports it)

If — and only if — the repo's stack has a built-in coverage tool the
project already uses, capture a before/after number so you can target the
thinnest areas and report the delta. Examples live in [[test-types]].
**Coverage is a compass, never the goal** (see [[test-coverage]]): use it
to find under-tested code, not as the success criterion. If the repo has
no coverage tooling, skip this — do not bolt on a new coverage framework
just to produce a number.

## 3. Write `.test_coverage.verify.sh`

Write an executable POSIX-sh script at the **workspace root** that runs
the build (compile) AND the relevant tests, and **exits non-zero on any
failure**. Shape (adapt to the repo — illustration, not a fixed command):

```sh
#!/bin/sh
set -e
devbox run -- task build
devbox run -- go test ./pkg/log/...
```

The deterministic gate re-runs **this exact file** and gates on its real
exit code — so it must genuinely pass, not merely look plausible. Keep it
POSIX-sh (the gate runs it via `sh`, which may be dash): no `[[ ]]`, no
`<<<`, no brace expansion, no `((i++))`.

If the repository genuinely has no test system you can plug into, write a
script that echoes that fact and exits 0, and say so plainly — but for a
**coverage** bot this is a near-certain sign you targeted the wrong place
or should bootstrap the repo's first test using its idiomatic runner.

## 4. Run it; fix what fails

Run your script. A failure is almost always your just-added test (a wrong
expectation, a missing import, a setup gap) — fix the **test** at the
source with the smallest change. Do **NOT** edit the code under test to
make a test pass (that defeats the purpose); the only exception is a
genuine, separately-flagged testability seam. Re-run until green.

## Safety — never destroy or recreate version-control state

Your script and edits run against the operator's real working tree —
often a **git worktree bind-mounted into a sandbox**. NEVER:

- `rm`, move, truncate, or recreate `.git` (or `.hg`/`.svn`), and NEVER
  `git init` / `git clone` over an existing checkout. A worktree's `.git`
  is a *file* pointing at the parent repo; deleting it or `git init`-ing
  over it **strands the operator's commits**.
- Reset, force-checkout, `git clean`, or otherwise discard tracked files
  or history.

If a test step needs git and git is **unavailable** in this environment,
guard it behind `git rev-parse --is-inside-work-tree` and note the skip —
a gate that skips a few git-dependent tests with a loud note is correct;
one that manufactures a repo to run them is not.

The verify script + its log are bot scratch — they are gitignored by the
target repo (or should be) and the commit step never stages them.
