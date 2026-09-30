---
name: verify-build
description: Detect and run a repository's OWN build + test so a deterministic gate can confirm the tree actually compiles and passes before the improve-loop commits. Stack-agnostic — read this when asked to verify a build.
---

# verify-build

Your job: make the repository in front of you **build and test green using its
own tooling**, and leave a re-runnable script for the deterministic gate.

This is the backstop for the whole-improve-loop review. Reviewers read **one
chunk of source at a time**, so a change that breaks compilation in *another*
file (a renamed or retyped symbol with a caller the current pass never exercised)
or a bug only a test catches is invisible to them. The build + tests are what
catch it — that is the entire reason this step exists.

## 1. Detect the repo's build + test commands

Do **not** assume a language. Look at the markers actually present and prefer
the project's own wrapper when it has one — a wrapper encodes the correct flags
and the correct toolchain:

- **Task runner / Makefile** — `Taskfile.yml` → `task build` / `task test` /
  `task check`; `Makefile` → `make build` / `make test`; `Justfile` → `just …`.
  This is almost always the right answer when present. List the targets that
  actually exist (`task --list`, `make -qp`, `just --list`) instead of assuming
  the conventional names: plenty of repos have `test` and `lint` but no `check`
  umbrella, and a verify.sh calling a target that does not exist fails for a
  reason that has nothing to do with the change under test.
- **A gate CI runs inline is still a gate.** When CI invokes something the task
  runner does not expose — a coverage threshold script, a schema check, a
  shell assertion — copy that invocation into verify.sh, one command per
  line (a chain loses every failure but its last's, §2): `task test`
  passing while CI's `go test …` then `bash hack/coverage.sh cover.out 85`
  fails is a green local verify on a red change, which is the exact
  failure the deterministic gate exists to prevent.
- **Pinned toolchain — honour it or the build fails on a version mismatch.**
  `devbox.json` → prefix with `devbox run -- …`. `.tool-versions` (asdf/mise),
  `.nvmrc`, `flake.nix`, `mise.toml` → activate accordingly. For Go: if
  `go.mod`'s `go` directive is newer than the installed `go version`, use the
  project's wrapper (e.g. `devbox run -- go …`) or `GOTOOLCHAIN=auto go …` so
  the pinned toolchain is fetched, rather than failing on "requires go >= X".
  - **devbox is first-class inside the iterion sandbox** — when the repo
    pins its toolchain via `devbox.json`, prefer `devbox run -- …`; the engine
    lays the whole `$HOME` subtree user-writable so the wrapper's cache/home
    work normally. (This wasn't always true: until the `homeNestedBindParents`
    fix, the Go-cache binds left `$HOME/.cache` root-owned and `devbox run`
    died with `mkdir: cannot create directory '/home/.../.cache/devbox':
    Permission denied` — observed 2026-06-23, run 019ef550. That root cause is
    fixed in the engine.) Last-resort only: if the wrapper still fails for a
    genuine environment reason (not a code error), fall back ONCE to the bare
    tool when it is on `PATH` — `command -v go || exit 1`, then `go build
    ./...` and `go test ./...`, each on its own line (§2: a failure before
    the last `&&` of a chain does not stop the script) — rather than
    retrying the wrapper. The default (slim) sandbox image
    ships only `node` and `python3` on `PATH` (no `go`, `cargo`, `python` or
    `pip`): when the tool is absent, report the environment failure instead of
    looping.
- **Go in a sandboxed git worktree: disable VCS stamping.** The run's
  workspace is a git *worktree* whose gitdir lives outside the sandbox
  mounts, so `go build`'s VCS probe can fail with `error obtaining VCS
  status: exit status 128` even though git itself works. Put
  `export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"` at the top
  of verify.sh (append — never clobber an inherited GOFLAGS) — the
  binary's stamped identity is irrelevant to a build+test gate.
  (Observed live 2026-08-19, run 01a01a51: the gate burned a pass on it.)
- **Language defaults (only when there is no wrapper):**
  - Go (`go.mod`): `go build ./...`, then `go test ./...`
  - Node (`package.json`): pick the package manager from the lockfile
    (`pnpm-lock.yaml`→pnpm, `yarn.lock`→yarn, `package-lock.json`→npm) and run
    its `build` + `test` scripts if defined.
  - Rust (`Cargo.toml`): `cargo build`, then `cargo test`.
  - Python (`pyproject.toml`/`setup.py`): the configured test runner, e.g.
    `uv run pytest` / `poetry run pytest` — through the repo's environment
    manager (a bare `python3 -m pytest` needs its dependencies installed) —
    plus a type/lint check if the project defines one.
  - Anything else: build + unit-test the way the repo's CI does — read
    `.github/workflows/*` (or other CI config) if present; CI is the source of
    truth for "how this repo is built".

Prefer the **fast** path: compile the whole module (a compile error is the
common breakage) + run the unit tests. Skip slow integration / e2e / live
suites unless they are the only tests the repo has.

## 1b. Include the repo's codegen-freshness / drift checks

Build + test green does NOT mean CI is green. Many repos commit **generated
artifacts** — an OpenAPI/Swagger spec + generated client types, protobuf/gRPC
stubs, generated mocks, a Helm chart version pinned to a package file — and
enforce in CI that the committed copy matches a fresh regeneration
(`regenerate && git diff --exit-code`; in verify.sh, the regen on its own
line, §2). A change that adds an API route, a
proto message, or a schema field but forgets to regenerate ships **green
build + red CI** — exactly the drift the deterministic gate or CI catches that
you should catch here instead.

So your `verify.sh` **must** mirror CI's gates, not just build+test — a build+test-only
`verify.sh` is the single most common way an autonomous change ships green-locally /
red-in-CI. This is not optional whenever the repo commits generated artifacts:

- **Read the CI config** (`.github/workflows/*.yml`, `.gitlab-ci.yml`,
  `.circleci/`) and include every gate it enforces — especially steps named
  *drift*, *generate*, *codegen*, *check*, *fmt*, *lint*, *tidy/verify*.
- **Grep the task runner** for freshness targets: `Taskfile.yml`/`Makefile`
  entries like `*:gen` / `*:generate` / `*:check` / `openapi:check` /
  `proto:check`. When a `check`/`verify` umbrella target exists that bundles
  lint + test + drift, prefer it — it is the repo's own definition of "CI
  green".
- The pattern to add for each committed-generated artifact: `<the repo's
  regen command>` on its own line, then `git diff --exit-code -- <the
  generated paths>` — a non-empty diff means stale, which is a real red,
  and so is a regen that fails (joined by `&&`, its failure would not stop
  the script: §2).

## 1c. Mirror CI's exact strictness — never stricter, never looser

CI is the reference in BOTH directions. Copying CI's *commands* but changing
their *thresholds* produces a verify.sh that disagrees with the repo's own
definition of green, and both signs of that disagreement have shipped a wrong
verdict in production:

- **Never stricter.** Do not invent failure thresholds the repo's CI does not
  enforce: no `--max-warnings 0` on a lint step whose CI run tolerates
  warnings, no promoting `go vet`/lint advisories to failures, no `-Werror`
  the build doesn't set. A tree with 500 pre-existing lint warnings that CI
  passes MUST pass verify.sh too — a stricter script red-flags a bump for
  debt it did not create (observed live: a vite bump held `hold_unstable`
  because verify.sh failed eslint on 534 pre-existing warnings / **0
  errors**, while the repo's CI on the same tree was fully green). Copy the
  exact invocation — flags included, one command per line (§2) — from the
  CI step or the task-runner
  target CI calls; when in doubt, run the repo's own umbrella target
  (`task check`, `make ci`) INSTEAD of hand-assembling steps.
- **Timeouts are a strictness axis — your sandbox is SLOWER than CI.** A
  wall-clock limit is a threshold like any other, and the verify sandbox
  (shared CPU, cold caches) routinely runs the same suite 2-5× slower than
  the repo's CI runners. Go's default per-binary `go test` timeout is 10m:
  a suite that takes 6m in CI dies at 10m in a throttled pod with
  `panic: test timed out` — a red that says nothing about the bump
  (observed live: four consecutive GitHub-Actions pin bumps held
  `hold_unstable` because the repo's e2e package outran 10m in the sandbox
  while the repo's CI was green on the identical shas). So: keep every
  explicit timeout CI sets, and RAISE — never lower — the implicit ones:
  pass a generous `-timeout` (e.g. 3× the CI duration, `-timeout 30m`) to
  `go test`, and give `verify.sh` itself headroom over the suite's worst
  observed wall-clock. A longer timeout is LESS strict and therefore always
  legitimate; a tighter one manufactures reds CI would not show.
- **Call the target; never transcribe its body.** When a task runner defines
  the step, `verify.sh` invokes `task <target>` — it does not copy what that
  target runs. A Taskfile/Makefile command is frequently a multi-line shell
  block, and a block does not survive being passed as one `sh -c` argument:
  its newlines and tabs arrive literal, the shell parses a single mangled
  line, and the script dies on a syntax error that has nothing to do with the
  dependency under test (observed live: a `gofmt` guard transcribed into
  `devbox run -- sh -c files=$(find …)` produced ``syntax error near
  unexpected token `then` `` and held a clean `@types/node` bump whose
  alignment was already correct). Transcription also silently drops the
  target's `dir:`, `env:` and prerequisites.
- **Never looser** is §1b: every gate CI enforces (drift, fmt, lint-as-error
  where CI makes it one) must be present — the precheck rejects a gateless
  script.

Litmus test before writing `exit`-relevant lines: "would the repo's CI, on
this exact tree, be red for this reason?" If you cannot point at the CI step
that fails, your script must not fail there either.

If you changed code that feeds a generator (a new HTTP route, a new exported
type in a schema-bearing package), regenerating and committing the output is
part of the work — the gate is here to force it. (iterion specifically:
`task openapi:check` + the helm chart drift check are CI gates; a new
`/api/...` route needs `task openapi:gen` committed.)

This section is **deterministically enforced** by the verify gate, and the
check is STRUCTURAL: it reads the script with comments stripped (a comment can
describe a gate; only a command can be one) and accepts any shape that really
fails the build on drift —

```sh
<regen> && git diff --exit-code || exit 1           # the probe's own status
if ! git diff --quiet -- <paths>; then exit 1; fi   # negated conditional,
                                                    # on one line or many
set -e; <regen>; git diff --quiet                   # fail-fast: it aborts
```

— and rejects a *commit-if-changed* block (`if ! git diff --quiet; then git
commit …; fi`), which ships the drift instead of failing on it. When the repo's
CI enforces a drift gate and `verify.sh` carries none of these, the gate fails
with DRIFT GATE MISSING (exit 3); a green verify that leaves new changes in the
tree fails with UNCOMMITTED REGEN OUTPUT (exit 4). Writing the gate here is
cheaper than being bounced by the enforcement.

## 1d. The Docker images CI builds are NOT in verify.sh — say so

Many repos build **Docker images in CI** (`Dockerfile`, `frontend/Dockerfile`,
docker-compose services). §1b and §1c tell you to mirror CI's gates; the image build is
the one you must NOT mirror:

- **Never build CI's images in verify.sh** (`docker build`, `docker compose
  build`), nor replay an image's dependency resolution: it needs pip (absent
  from the default image), pips before 25.3 download every wheel in full (a
  torch pin alone is 800 MB), and its verdict depends on the local
  interpreter and on which indexes the image reads — only CI's own build
  can say (below).
- **A docker command a test suite needs** (`docker compose up -d db`, or a
  wrapper target that runs one — `task test`, `make test`: read its
  commands first) stays out too, whether or not Docker answers: run the
  target's non-docker steps directly — the one exception to calling a
  target rather than transcribing it — and name the suite that leaves out
  as not covered, like an image. The iterion sandbox has no Docker CLI or
  daemon by design (no socket, no `--privileged`), so there the step would
  fail on every pass; on an unsandboxed host a compose service is the
  operator's shared state — `-p` isolates neither a published port nor a
  `container_name`, a killed run skips its teardown, and a teardown that
  names no project removes the operator's own. CI runs that suite.
- Instead, **name each image CI builds — and each suite you left out — as
  not covered** by the green gate, in your summary AND as the FIRST line
  verify.sh prints (`echo "NOT COVERED: image <Dockerfile path>, built by
  CI only"` before the checks — never after them: a command after the
  last check becomes the script's exit status and hides that check's
  failure). The deterministic gate lifts every `NOT COVERED:` line into
  the log tail of a green run. The image
  build installs the dependency set from the index the
  CI actually uses, which may not carry a version your sandbox already has —
  only CI's own build can say. (Paid: a transitive pin, dragged in by a
  never-imported dependency, was no longer carried by the index the CI
  resolves against — every api image build failed while the sandbox suite
  stayed green. The pipeline's result is the signal; a green sandbox gate says
  nothing about it.)

## 2. Write the verify script to the scratch dir

Write an executable POSIX-sh script at the exact `verify.sh` path your task
prompt gives you — an **out-of-tree scratch dir** (`scratch_dir`), NOT the
target repo. Create that directory first if it does not exist (`mkdir -p` its
parent). The script runs the build AND tests you settled on and **exits
non-zero on any failure**. Example
*shape* (adapt to the repo — illustration, not a fixed command):

```sh
#!/bin/sh
set -e
devbox run -- task build
devbox run -- task test
# Codegen-freshness gate (§1b) — regenerate the repo's committed derived
# artifacts, then fail if the tree drifted. Adapt the regen command(s) to the
# repo (grep the task runner for *:gen / *:generate); omit this block only when
# the repo commits NO generated artifacts. `git diff --exit-code` (no paths)
# catches drift in whatever the regen wrote.
devbox run -- task openapi:gen      # ← the repo's own regen target(s)
git diff --exit-code || { echo "codegen drift — regenerate + commit the output" >&2; exit 1; }
```

**A check gates only as a plain top-level command, or ended by `|| exit
1`** — and in a pipeline only its last stage gates, `|| exit 1` or not
(below). `set -e` ignores a failure in every command of an AND-OR list but
the last (`a && b`, `a || echo …`), in an `if`/`while` condition, anywhere
inside a `{ }`, `( )` or function placed there (even with `set -e`
re-armed inside), in a child shell (`sh -c 'a; b'`), and in a command
substitution used as an argument (`echo "$(…)"`, `export V=$(…)`,
`test -z "$(…)"` — assign first: `s=$(…)` alone carries the status), and
in a pipeline negated with `!` (`! grep -q it.only tests/` never stops the
script: write `if grep -q it.only tests/; then exit 1; fi`). So
`command -v go && go build ./... && go test ./...` followed by any other
command exits 0 with nothing built. Put each check on its own line, or
end its chain with `|| exit 1`; give the capture convention below ONE
command, never a group. A step that must run in a directory stays one
subshell line — `(cd web && npm ci && npm test)`: its status is the
chain's, it gates, and the `cd` does not leak into the lines after it.

The deterministic gate re-runs **this** script and gates the commit on its real
exit code — so it must genuinely pass, not merely look plausible. A `verify.sh`
that omits the §1b regen step when the repo has generated artifacts is the
canonical way a change lands green-locally / red-in-CI.

### Capture the status, then filter — never pipe a gate

A pipeline exits with its **last** command's status. `go test ./... 2>&1 | tail
-10` therefore reports `tail`, and a build whose compiler is not even installed
reads as a pass (measured: a missing `tsc` returned 127 and the run recorded
`EXIT=0`). `set -o pipefail` is **not POSIX** and `verify.sh` runs under
`/bin/sh` — dash does not have it — so the convention is:

```sh
<check> >step.log 2>&1 || { tail -50 step.log >&2; exit 1; }   # gate, then filter
V=$(<producer> | head -1)                                      # a captured VALUE is fine
<noisy-but-not-a-gate> | tail -5 || true                       # explicitly not a gate
```

Read `$?` from the command itself, never from an `echo` after a pipeline. A
check whose tool is missing is **unavailable**, not passed — say so in the
summary rather than letting it read green.

This is **deterministically enforced**: the gate refuses a `verify.sh` that
pipes a top-level command into an output filter (`tail`/`head`/`tee`/`wc`/a
filtering `grep`) with **MASKED EXIT STATUS** (exit 5) *before* running it, and
moves the script aside so it is regenerated. Comments are stripped first, and
the three shapes above are never flagged.

**Write the script even when the bump needed no alignment.** "Nothing changed
in the code, so there is nothing to verify" is the reasoning to refuse: the
gate proves the repo still builds with the new dependency, which is exactly the
question a base-image digest refresh or a lockfile-only bump raises. A run that
produces no script proves nothing and cannot commit — it is sent back to write
one, and after two attempts the PR is held on a verdict that says the build was
never established. §1b applies here too: what the script covers is decided by
the repo's CI, never by which files the bump happened to touch.

A script that runs **nothing** is rejected the same way. An empty or
comment-only script exits 0, which the gate reads as a verified build — so
"write one" is never satisfied by writing a placeholder. Shell no-ops (`set`,
`cd`, `exit`, `true`) do not count as running anything.

**Every line must be a COMMAND.** Never emit a bare path to a source file: the
shell tries to execute it, answers `Permission denied`, and the run reports a
red build that never ran. To exercise a set of files, pass them to the repo's
own runner (`go test ./...`, `pytest tests/`, `npm test`) — the runner takes
paths, the shell does not. Nothing catches this for you: a script of paths runs,
fails on every line, and reports a red build for a bump that broke nothing —
which an operator then has to read the log to disbelieve.

## 3. Run it, and fix what the just-applied changes broke

Run your script. If it fails, the failure is almost always introduced by the
changes the preceding work just applied. Read the error and fix it **at the
source** with the **smallest** change that restores build + tests: update the
missed caller, correct the signature, fix the read/write mode, repair the test.
Do **not** refactor or change behaviour beyond what is needed to compile and
pass. Re-run until green.

If the repository genuinely has no build/test system (pure docs/config), write
a script that echoes that fact and exits 0, and say so plainly in your summary —
do not invent a build.

## Safety — never destroy or recreate version-control state

The verify script and your fixes run against the operator's real working tree —
often a **git worktree bind-mounted into a sandbox**. NEVER:

- `rm`, move, truncate, or recreate `.git` (or `.hg`/`.svn`), and NEVER run
  `git init` / `git clone` over an existing checkout. A worktree's `.git` is a
  *file* pointing at the parent repo; deleting it or `git init`-ing over it
  **disconnects the operator's worktree and strands their commits** (observed
  2026-06-15, run 019eca0d — a bootstrap that ran `rm -f .git; git init` severed
  the worktree's link to the repo).
- Reset, force-checkout, `git clean`, or otherwise discard tracked files or
  history.

If a build/test step needs git and git is **unavailable in this environment**
(e.g. `git rev-parse` fails because a worktree's `.git` target isn't mounted in
the sandbox), do NOT manufacture a repo. Make the verify script **skip** the
git-dependent tests — guard them behind `git rev-parse --is-inside-work-tree` —
and note the skip in your summary. A gate that skips a few git-dependent tests
with a loud note is correct; one that destroys the operator's repo to run them
is not.
