---
name: package-managers
description: |
  Reference for package-manager operations across common ecosystems —
  discovery (what's outdated), upgrade (bump one or many), install
  (refresh lockfile), audit (vulnerability scan). NOT prescriptive:
  the actual repo decides which manager is in use; treat each
  section as a starting point and adapt to what's actually
  installed. Refer to this from discover_outdated, upgrade,
  install, batch_upgrade_patches, and security_audit nodes.
---

# Package manager operations

Reference for upgrading, installing, discovering outdated, and
auditing dependencies across the ecosystems typically encountered in
renovacy runs. Treat each section as a hint — always inspect the
actual workspace first to identify which manager variant is in use,
probe the candidate CLI (`<manager> --version`), and adapt to the
local conventions before running a command.

## Workflow

1. **Detect**: identify the manager variant from the lockfile /
   manifest / vendored shim (see "Detection by lockfile" below).
2. **Probe**: `<manager> --version` to confirm the binary resolves.
   If not, try a shim (`corepack`, vendored release, `python -m`,
   `uvx`, …).
3. **Run**: pick the operation's section (discover / upgrade /
   install / audit) and use the closest match. Bulk-friendly
   managers accept a multi-spec argument list in one command; a
   few require per-spec invocations (noted in each section).
4. **Adapt**: if the command fails, surface the stderr verbatim and
   STOP. Don't retry with a "looks similar" command — the most-
   correct CLI failing is a meaningful signal the operator needs
   to see.
5. **Cover gaps**: if you encounter an ecosystem not listed here,
   apply the same general procedure (detect → probe → run → verify)
   from official docs, and note it in your output so the operator
   can extend this skill.

## Detection by lockfile

| Lockfile / shim                       | Most likely manager     |
|---------------------------------------|-------------------------|
| `yarn.lock` + `.yarn/releases/*.cjs`  | yarn berry              |
| `yarn.lock` + `yarn --version` < 2    | yarn classic            |
| `pnpm-lock.yaml`                      | pnpm                    |
| `bun.lockb`                           | bun                     |
| `package-lock.json` (no yarn.lock)    | npm                     |
| `go.sum`                              | go modules              |
| `poetry.lock`                         | poetry                  |
| `uv.lock`                             | uv                      |
| `Pipfile.lock`                        | pipenv                  |
| `requirements*.txt` only              | pip (manual pins)       |
| `Cargo.lock`                          | cargo                   |
| `composer.lock`                       | composer                |
| `Gemfile.lock`                        | bundler                 |
| `mix.lock`                            | mix (elixir)            |
| `pom.xml`                             | maven                   |
| `build.gradle*` + `gradle/wrapper`    | gradle                  |

If multiple lockfiles coexist (e.g. `yarn.lock` AND `package-lock.json`),
read the project's CI / scripts to see which one the team actually
maintains. The maintained one wins; the others are stale and not
authoritative.

## Dependencies that are not packages

A package manager is not the only thing that pulls foreign code into a
build. These four are updated by the same bots, bump the same way, and are
exactly as able to execute code you did not review — treat them with the
same rigour, not as "config changes".

| File | What moved | Where the truth is |
|---|---|---|
| `Dockerfile` / `Containerfile` | base-image tag or `@sha256:` digest | the registry manifest + the image's own layers |
| `devbox.json` / `devbox.lock` | a Nix package pin | nixhub.io / the nixpkgs commit in the lock |
| `Chart.yaml` / `values*.yaml` | a chart version or an image tag | the chart repo + the image it references |
| `.github/workflows/*`, `action.yml` | an Action pinned by tag or SHA | the action's repo at that ref |

### Container image digests

A digest bump is *usually* a rebuild of the same version — and that is
precisely why it is worth checking rather than waving through: the tag did
not change, so nothing else signals that the contents did.

Resolve what actually moved:

```sh
# The bot's own devbox provides crane; the sandbox image has no registry
# client of its own.
crane digest golang:1.26                 # what the tag points at NOW
crane manifest golang:1.26@sha256:<old>  # the pinned one
crane config  golang:1.26@sha256:<new> | jq '.history[-3:], .config.Entrypoint, .config.User'
```

What to look at, in order: does the new digest still correspond to the tag
the Dockerfile names (a digest that no tag resolves to is a red flag); did
`User`, `Entrypoint` or `Cmd` change (a base image that stops dropping root
is a real regression); did the last history entries gain a step that fetches
something at build time. `trivy image --scanners vuln <ref>` gives the CVE
delta between old and new — run it on both and diff, since the interesting
answer is what the bump *fixes* versus what it *introduces*.

If no registry client resolves, say so in the summary rather than implying
you inspected the image. An unverified digest is a real caveat, not a detail.

### Nix / devbox pins

`devbox.json` carries the human-readable pin (`jq@1.8.2`); `devbox.lock`
carries the resolved nixpkgs commit — the lock is what actually determines
the bits. Check the version really exists on nixhub for that package, and
that the lock's `resolved` URL and the version in `devbox.json` agree; a
lock pointing at a commit unrelated to the stated version is the signal
worth escalating. `devbox install --dry-run` (or `devbox info <pkg>`)
confirms the pin resolves at all.

### Helm charts

A chart bump can change rendered manifests far beyond the version string.
`helm template` the old and new charts with the repo's own values and diff
the output — that is the real change set. Pay attention to anything that
touches RBAC, `securityContext`, `hostPath`/`hostNetwork`, or a new image
reference: a chart that quietly grants a ClusterRole is a supply-chain event
even when the package itself is fine.

### Pinned Actions

An Action pinned by SHA that moves to another SHA is a code change in a
build step that already holds a token. Read the diff between the two refs
(`gh api repos/<owner>/<repo>/compare/<old>...<new>`), and give particular
weight to changes in what it exfiltrates, what it writes, and any new
network call. A tag that was re-pointed to a different commit without a
release is the classic Action-hijack shape.

## Cache locations — keep them OUT of the workspace

Most package managers default a cache (downloaded archives, compiled
artefacts, toolchain bits) to a path derived from `$HOME`. Inside the
sandbox, when a manager can't resolve a writable `$HOME` it silently
falls back to a path adjacent to `pwd` — i.e. **inside the workspace**.
A single missed env var here means the next commit-prep step will try
to enumerate millions of untracked cache files via
`git status` / `git ls-files --others` and crash the Node tool's
default 1 MB `execSync` buffer (observed once: a `go/pkg/mod/...`
tree of ~1 050 864 chars of path listing failed `prepare_commit`
with `exit status 1`).

**Always export these env vars before running any toolchain command:**

- **Go**: `GOPATH`, `GOCACHE`, `GOMODCACHE` (default `$HOME/go`,
  `$HOME/.cache/go-build`, `$GOPATH/pkg/mod`)
- **Rust / Cargo**: `CARGO_HOME` (default `$HOME/.cargo`)
- **Python / pip / uv**: `XDG_CACHE_HOME`, `PIP_CACHE_DIR`,
  `UV_CACHE_DIR` (default `$HOME/.cache/...`)
- **Node / npm / yarn / pnpm**: `npm_config_cache` (default
  `$HOME/.npm`); `pnpm` honours `XDG_CACHE_HOME`; yarn berry's local
  `.yarn/cache` is intentionally per-project — leave that one alone
- **Generic**: `TMPDIR` so commands spawning their own scratch
  (tar, jq, postinstall hooks) land outside the workspace too

The bot's sandbox already pins all of the above to `/tmp/iterion-renovacy/...`
in the workflow's `sandbox.env` block. If you are running a command
outside the sandbox (host-side fallback, devbox shell, …) and there's
any doubt about whether `$HOME` is writable, prefix the command with
the env explicitly:

```
env GOPATH=/tmp/iterion-renovacy/go \
    GOCACHE=/tmp/iterion-renovacy/go-build \
    GOMODCACHE=/tmp/iterion-renovacy/go-mod \
    TMPDIR=/tmp/iterion-renovacy/tmp \
  go get example.com/m@v1.2.3 && go mod tidy
```

When you discover a new manager whose cache lands in the workspace by
default, ADD an entry above so the next agent doesn't relearn it.

## Discover outdated packages

Each manager emits "what's outdated" in its own JSON shape; you'll
need to normalise the output to the schema `discover_outdated`
expects (`{name, current, target, risk, ecosystem, kind, dep_type,
workspace}`).

- **yarn classic:** `yarn outdated --json`
- **yarn berry:** `yarn outdated --json` doesn't exist; query the
  lockfile + `npm view <pkg> versions` per package, or `yarn npm info`.
- **npm:** `npm outdated --json` (object-keyed by name; transpose to array)
- **pnpm:** `pnpm outdated --format=json`
- **bun:** `bun outdated --json` if available, else lock + registry probe
- **cargo:** `cargo outdated --format=json` (the cargo-outdated plugin;
  install if missing or fall back to `cargo update --dry-run` parsing)
- **pip:** `pip list --outdated --format=json`
- **poetry:** `poetry show --outdated --top-level --json`
- **uv:** `uv pip list --outdated --format=json`
- **pipenv:** `pipenv lock --keep-outdated` then parse Pipfile.lock vs Pipfile
- **composer:** `composer outdated --format=json`
- **bundler:** `bundle outdated --parseable`
- **go modules:** `go list -u -m -json all` (filter `Update != null`)
- **maven:** `mvn versions:display-dependency-updates`
- **gradle:** `gradle dependencyUpdates` (with the
  `com.github.ben-manes.versions` plugin)
- **mix (elixir):** `mix hex.outdated`

Monorepo / workspaces: most managers have a `--filter <ws>` /
`workspace <ws>` form. Use the workspace hint to scope discovery
correctly; merge duplicate packages by name keeping the entry whose
`current` lags the most.

**Target = latest stable**, NOT clipped to the project's declared
range. The downstream scope policy is what decides patch/minor/major;
clipping here makes majors unreachable forever.

## Upgrade one or many packages

Bulk-friendly managers accept multi-spec lists. Cargo doesn't —
chain per-crate.

- **yarn classic:** `yarn upgrade <name>@<target> [<name>@<target> …]`
- **yarn berry:** `yarn up <name>@<target> …`. May need `corepack
  yarn ...` or `node .yarn/releases/yarn-X.Y.Z.cjs ...`.
- **pnpm:** `pnpm update <name>@<target> …`
- **bun:** `bun update <name>@<target> …`
- **npm:** `npm install <name>@<target> …` (npm doesn't distinguish
  install vs upgrade). `--save-exact` to pin.
- **go modules:** `go get <module-path>@<target>`. Respect the `v`
  prefix per semver (e.g. `v1.2.3`). After: `go mod tidy`.
- **pip:** edit the pin in `requirements.txt`/`pyproject.toml`, then
  `pip install --upgrade <name>==<target>`.
- **poetry:** `poetry add <name>@<target>` (or `^<target>` for range).
  Preserves dependency group.
- **uv:** `uv add <name>==<target>` for project deps, or
  `uv pip install --upgrade <name>==<target>` for pip-shim path.
- **pipenv:** `pipenv install <name>==<target>` (writes to Pipfile).
- **cargo:** `cargo update -p <name> --precise <target>` per crate.
  Chain with `&&` for multiple crates (cargo doesn't accept multi-crate
  bulk on `update`). For incompatible majors, edit `Cargo.toml` first
  then run `cargo update`.
- **composer:** `composer require <name>:<target>`. Preserves
  `require-dev` placement.
- **bundler:** edit the Gemfile pin, then
  `bundle update <name> --conservative`.
- **mix (elixir):** `mix deps.update <name>`.
- **maven:** edit `<version>` element in `pom.xml`. Verify with
  `mvn -q dependency:tree`. Bulk = scripted XML edits + one verify.
- **gradle:** edit the version literal in `build.gradle*`. Verify
  with `gradle dependencies --refresh-dependencies`.

Monorepo: many JS managers accept `<manager> workspace <ws> ...`. Pick
the right scope from project layout.

## Install / refresh lockfile after a manifest change

Run AFTER an upgrade (or any manifest edit) so transitive deps
resolve and the lockfile is rewritten.

- **yarn:** `yarn install` (classic) / `yarn install --immutable`
  (berry, CI mode)
- **npm:** `npm install` (writes `package-lock.json`)
- **pnpm:** `pnpm install`
- **bun:** `bun install`
- **go modules:** `go mod tidy` (refreshes `go.sum`)
- **pip:** `pip install -r requirements.txt`
- **poetry:** `poetry lock --no-update` (re-lock without bumping) or
  `poetry install` (lock + install)
- **uv:** `uv sync` (re-lock + install) or `uv lock` (lock only)
- **pipenv:** `pipenv install` or `pipenv lock`
- **cargo:** `cargo build` or `cargo check` (re-resolves)
- **composer:** `composer update --lock` or `composer install`
- **bundler:** `bundle install`
- **mix:** `mix deps.get`
- **maven:** `mvn dependency:resolve` (refreshes the local cache)
- **gradle:** `gradle dependencies --refresh-dependencies`

## Family fast-path catalogue (machine-readable — consumed by family_upgrade / family_validate / family_revert)

The deterministic Phase-2 "family" tools read the JSON block below to
dispatch by package manager — there is NO per-manager `case` in the
workflow DSL. Adding or amending a manager is a skill edit here; the
`.bot` graph never changes. Each entry:

- `aliases` — every `pkg_manager` id that maps to this entry (lower-cased
  on lookup).
- `spec_form` — how a `name`+`target` pair becomes one upgrade spec:
  `@` → `name@target`, `==` → `name==target`, `:` → `name:target`,
  `per-package` → one full `upgrade` invocation per package (joined `&&`),
  filling `{name}` / `{version}`.
- `upgrade` — bulk upgrade command; `{specs}` is the space-joined spec
  list (or `{name}`/`{version}` for `per-package`).
- `install` — resolve/refresh after the upgrade (the family_validate
  install gate; a conflict here fails the family).
- `smoke` — post-install smoke strategy: `compiled` runs `smoke_cmd`
  (build/typecheck); `resolve` treats a clean install as the smoke
  (interpreted langs); `node-script` runs the first of
  typecheck/tsc/build/test present in package.json via `script_run`
  (`{script}` placeholder).
- `revert_install` — reinstall after `git reset --hard` on a failed
  family (node managers only; others need no reinstall).
- `kind: "manifest-surgery"` — no clean one-shot pin CLI; the family is
  deferred to the per-package agentic loop (graceful, not skipped).

<!-- iterion:pkgmgr
[
  {"aliases":["yarn","yarn-berry","yarn3","yarn4"],"spec_form":"@","upgrade":"yarn up {specs}","install":"yarn install","smoke":"node-script","script_run":"yarn {script}","revert_install":"yarn install"},
  {"aliases":["npm"],"spec_form":"@","upgrade":"npm install {specs}","install":"npm install","smoke":"node-script","script_run":"npm run {script}","revert_install":"npm install"},
  {"aliases":["pnpm"],"spec_form":"@","upgrade":"pnpm update {specs}","install":"pnpm install","smoke":"node-script","script_run":"pnpm {script}","revert_install":"pnpm install"},
  {"aliases":["bun"],"spec_form":"@","upgrade":"bun update {specs}","install":"bun install","smoke":"node-script","script_run":"bun run {script}","revert_install":"bun install"},
  {"aliases":["go","go-modules","gomod"],"spec_form":"@","upgrade":"env GOPATH=${GOPATH:-/tmp/iterion-renovacy/go} GOCACHE=${GOCACHE:-/tmp/iterion-renovacy/go-build} GOMODCACHE=${GOMODCACHE:-/tmp/iterion-renovacy/go-mod} go get {specs} && env GOPATH=${GOPATH:-/tmp/iterion-renovacy/go} GOCACHE=${GOCACHE:-/tmp/iterion-renovacy/go-build} GOMODCACHE=${GOMODCACHE:-/tmp/iterion-renovacy/go-mod} go mod tidy","install":"env GOPATH=${GOPATH:-/tmp/iterion-renovacy/go} GOCACHE=${GOCACHE:-/tmp/iterion-renovacy/go-build} GOMODCACHE=${GOMODCACHE:-/tmp/iterion-renovacy/go-mod} go mod download","smoke":"compiled","smoke_cmd":"env GOPATH=${GOPATH:-/tmp/iterion-renovacy/go} GOCACHE=${GOCACHE:-/tmp/iterion-renovacy/go-build} GOMODCACHE=${GOMODCACHE:-/tmp/iterion-renovacy/go-mod} go build ./... && env GOPATH=${GOPATH:-/tmp/iterion-renovacy/go} GOCACHE=${GOCACHE:-/tmp/iterion-renovacy/go-build} GOMODCACHE=${GOMODCACHE:-/tmp/iterion-renovacy/go-mod} go vet ./..."},
  {"aliases":["cargo"],"spec_form":"@","upgrade":"env CARGO_HOME=${CARGO_HOME:-/tmp/iterion-renovacy/cargo} cargo add {specs}","install":"env CARGO_HOME=${CARGO_HOME:-/tmp/iterion-renovacy/cargo} cargo fetch","smoke":"compiled","smoke_cmd":"env CARGO_HOME=${CARGO_HOME:-/tmp/iterion-renovacy/cargo} cargo check"},
  {"aliases":["poetry"],"spec_form":"@","upgrade":"poetry add {specs}","install":"poetry install --no-interaction","smoke":"resolve"},
  {"aliases":["pip","pip-poetry"],"spec_form":"==","upgrade":"python3 -m pip install --upgrade {specs}","install":"python3 -m pip check","smoke":"resolve"},
  {"aliases":["uv"],"spec_form":"==","upgrade":"uv add {specs}","install":"uv sync","smoke":"resolve"},
  {"aliases":["composer"],"spec_form":":","upgrade":"composer require --no-interaction --no-scripts {specs}","install":"composer install --no-interaction --no-scripts","smoke":"resolve"},
  {"aliases":["nuget","dotnet"],"spec_form":"per-package","upgrade":"dotnet add package {name} --version {version}","install":"dotnet restore","smoke":"compiled","smoke_cmd":"dotnet build --nologo"},
  {"aliases":["bundler","maven","gradle","mix","swift","nimble","pub","conan","hex"],"kind":"manifest-surgery"}
]
-->

## Vulnerability audit

JSON-emitting auditors are preferred so the agent can parse findings
without LLM-side text scraping.

- **yarn classic:** `yarn audit --json`
- **yarn berry:** `yarn npm audit --json --recursive`
- **npm:** `npm audit --json`
- **pnpm:** `pnpm audit --json`
- **bun:** `bun audit --json` (Bun 1.2+); otherwise `bun pm pack
  --dry-run` + osv lookup, or fall back to npm-audit semantics on
  the companion `package-lock.json`.
- **cargo:** `cargo audit --json` (install via `cargo install cargo-audit`
  if missing)
- **pip:** `pip-audit --format json`
- **poetry:** `poetry export -f requirements.txt | pip-audit
  --requirement /dev/stdin --format json`
- **uv:** `uv export --format requirements-txt | pip-audit
  --requirement /dev/stdin --format json`
- **composer:** `composer audit --format json`
- **bundler:** `bundle audit check --update` (gemfile audit)
- **go:** `govulncheck -json ./...`
- **mix:** `mix deps.audit` (hex_audit) + `mix hex.audit` for
  retired packages
- **maven:** `mvn -q org.owasp:dependency-check-maven:check
  -Dformats=JSON -DfailBuildOnCVSS=11`
- **gradle:** `gradle dependencyCheckAnalyze -PoutputFormat=JSON`
  (OWASP plugin)

**Cross-check with OSV-scanner** when a lockfile is available (covers
SCA / malware angle across every ecosystem):

```
npx --yes osv-scanner --lockfile=<path> --format json
```

Secondary sources to confirm a primary auditor's finding (don't burn
budget chasing every one — use ONE secondary when the primary
surfaced something):

- **JS:** retire.js (`npx --yes retire --outputformat json`) for
  known-vulnerable libs, AND socket.dev (`SOCKET_API_KEY` env)
  for typosquat / install-script risk
- **Python:** safety (`pip install safety && safety check --json`)
- **Rust:** `cargo deny check advisories` (adds licence + duplicate
  gates on top of cargo audit)
- **Go:** osv-scanner against `go.sum` (govulncheck is primary)
- **PHP / Ruby / Maven / Gradle / mix:** osv-scanner is the
  universal cross-check

## Shim fallbacks when the bare binary doesn't resolve

- corepack: `corepack enable` once (or `corepack <yarn|pnpm> ...`)
- vendored releases: `node .yarn/releases/yarn-X.Y.Z.cjs ...`
- python: `python -m poetry`, `python -m pip`, `python -m pip_audit`
- uvx: `uvx <tool>` for python tools not installed in the env
- pipx: `pipx run <tool>`
- volta / asdf / nvm / pyenv / rbenv / sdkman: respect the project's
  `.tool-versions` / `.nvmrc` / `.python-version` / `.ruby-version`

If the manager isn't installed in the sandbox at all, **fail
explicitly** — don't silently swap to a different one. The operator
needs to see the missing-tooling signal to decide whether to add it
to the container's post_create.

## When this skill doesn't cover the ecosystem

Iterion is meant to handle ANY tech stack. If you encounter an
ecosystem not listed here:

1. Inspect the workspace for the canonical lockfile / manifest.
2. Look up the ecosystem's official upgrade / audit / install docs
   (the `bash` tool can fetch via `curl` if registry docs are
   reachable).
3. Apply the same general procedure (probe → run → verify).
4. Once you've discovered a working approach, surface a brief note
   in your output so the operator can fold it back into this skill.
