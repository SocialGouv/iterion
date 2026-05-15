---
name: package-manager-upgrades
description: |
  Starting points for upgrading dependencies across common package
  manager ecosystems. NOT prescriptive: the actual repo decides
  which manager is in use; this skill is a reference, not a
  recipe to apply blindly. Refer to it from any upgrade /
  install / batch-upgrade step.
---

# Package manager upgrade conventions

Reference for upgrading one or many packages across the ecosystems
typically encountered in renovacy / dependency-bump runs. Treat each
section as a hint — always inspect the actual workspace first to
identify which manager variant is in use, probe the candidate CLI
(`<manager> --version`), and adapt to the local conventions before
running an upgrade.

## How to use this skill

1. Group the packages you need to upgrade by their ecosystem.
2. For each ecosystem:
   - Detect the manager variant from the lockfile / manifest /
     vendored shim (see "Detection by lockfile" below).
   - Pick the closest match from the corresponding section.
   - Adapt the command to the actual binary path, workspace flag,
     pinning convention.
   - Probe `--version` (or equivalent) before invoking. If the
     binary doesn't resolve, try a shim (`corepack`, vendored
     release, `python -m <manager>`, `uvx`, etc.).
3. If no variant fits, fail explicitly and report what you saw —
   silently swapping to a similar manager hides the real issue.

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
maintains.

## JavaScript / TypeScript

Bulk upgrades are usually supported in one command. Workspace flag
varies — many of these support `<manager> workspace <ws> ...`.

- **yarn classic:** `yarn upgrade <name>@<target> [<name>@<target> ...]`
- **yarn berry:** `yarn up <name>@<target> ...` ; may need
  `corepack enable` + `corepack yarn ...`, or
  `node .yarn/releases/yarn-X.Y.Z.cjs ...` if the bare binary doesn't
  resolve in the sandbox.
- **pnpm:** `pnpm update <name>@<target> ...`
- **bun:** `bun update <name>@<target> ...`
- **npm:** `npm install <name>@<target> ...` (npm doesn't distinguish
  install vs upgrade)

## Go

- Module path with `@version`: `go get <module-path>@<target>`
- Respect the `v` prefix on semver versions (e.g. `v1.2.3`). For
  pseudo-versions, pass them verbatim.
- After upgrading: `go mod tidy` so `go.sum` is consistent.

## Python

- **pip + `requirements.txt`:** edit the pin to `<name>==<target>`,
  then `pip install --upgrade <name>==<target>` (or
  `pip install -r requirements.txt --upgrade`).
- **poetry:** `poetry add <name>@<target>` (or `^<target>` to
  preserve range). Preserves the dependency group from the manifest.
- **uv:** `uv add <name>==<target>` for project deps, or
  `uv pip install --upgrade <name>==<target>` for the pip-shim path.
- **pipenv:** `pipenv install <name>==<target>` (writes to Pipfile).
- **hatch / pdm:** edit `pyproject.toml`'s pin, then run the
  manager's lock-refresh (`hatch dep update`, `pdm lock`).

## Rust

- **cargo:** `cargo update -p <name> --precise <target>` per crate.
  cargo does NOT accept a multi-crate bulk argument list on `update`,
  so chain calls (e.g. with `&&` or a loop).
- If the upgrade requires a manifest change (incompatible major,
  feature flag toggle), edit `Cargo.toml` first, then run
  `cargo update` to refresh `Cargo.lock`.

## Java / JVM

- **maven:** edit the `<version>` element in `pom.xml`. Verify with
  `mvn -q dependency:tree`. Bulk: scripted XML edits + one tree run.
- **gradle:** edit the version literal in `build.gradle*`. Verify
  with `gradle dependencies --refresh-dependencies`.

## Others

- **composer (PHP):** `composer require <name>:<target> ...`.
  Preserve `require-dev` placement.
- **bundler (Ruby):** edit the Gemfile pin, then
  `bundle update <name> --conservative`.
- **mix (Elixir):** `mix deps.update <name>`.
- **OS packages (apt / dnf / pacman):** when the repo pins OS-level
  packages (e.g. in a Dockerfile or devcontainer.json), edit the pin
  per the file's convention — there's no universal upgrade command.

## Cross-cutting hints

- **Monorepo / workspaces:** most JS managers accept a workspace
  flag (`yarn workspace <ws> up ...`, `pnpm --filter <ws> update ...`).
  Pick the right scope from project layout.
- **Probing first:** `--version` is cheap. If you can't even resolve
  the manager, you can't upgrade — fall back to shims OR fail loud.
- **Don't chain install / build / test from an upgrade step.** Those
  are separate downstream nodes. The upgrade step writes manifests
  and lockfiles only.

## When this skill doesn't cover the ecosystem

Iterion is meant to handle ANY tech stack. If you encounter an
ecosystem not listed here:

1. Inspect the workspace for the canonical lockfile / manifest.
2. Look up the ecosystem's official upgrade documentation (the
   `bash` tool can fetch via `curl` if registry docs are reachable).
3. Apply the same general procedure (probe → run → verify).
4. Once you've discovered a working approach, the operator may want
   to extend this skill — surface a brief note in your output so
   they can fold it back into the bundle.
