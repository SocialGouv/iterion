---
name: devbox-setup
description: |
  Playbook for authoring a pinned devbox.json that reproduces a repo's
  build/test/e2e toolchain, for use by Devboxing (devbox-setup) and by any bot
  bootstrapping a project's dev environment (ADR-017 Tier-2/Tier-3).
---

# devbox-setup — author a project's devbox.json

Goal: a **pinned, reproducible** `devbox.json` at the repo root that supplies
exactly the toolchain needed to **build, test, and e2e** the project — so
`devbox run -- <cmd>` runs project commands in a deterministic environment.
Write ONLY `devbox.json` (+ the `devbox.lock` that `devbox install`
generates). Never touch source.

## 1. Detect the stack (from manifests, not guesses)

Read the manifests present at the repo root (and obvious subdirs):

| Manifest | Toolchain to add |
|---|---|
| `go.mod` | `go` (match the `go` directive's major.minor) |
| `package.json` | `nodejs` (+ the package manager: `pnpm`/`yarn`/`npm` from the lockfile / `packageManager` field) |
| `pyproject.toml` / `requirements.txt` / `Pipfile` | `python3` (+ `poetry`/`uv`/`pipenv` if used) |
| `Cargo.toml` | `rustc` + `cargo` |
| `Gemfile` | `ruby` |
| `pom.xml` / `build.gradle*` | `jdk` + `maven`/`gradle` |
| `Dockerfile`/`docker-compose` | usually NOT in devbox (runtime), but note services |

Pin to the version the code actually targets: the `go` directive, Node from
`engines.node` / `.nvmrc` / `.tool-versions`, Python from `requires-python`.
Prefer a real version over `@latest` so scans are reproducible — `devbox
install` records the resolved nixpkgs commit in `devbox.lock`.

## 2. Detect build / test / e2e commands

- **Build**: `go build ./...`; npm/pnpm `build` script; `cargo build`; etc.
- **Test**: `go test ./...`; the `test` script; `pytest`; `cargo test`.
- **E2E**: look for `@playwright/test` / `cypress` / `puppeteer` in
  `package.json` deps or a `test:e2e` script. Playwright also needs its
  **browsers**: add an `init_hook` running `npx playwright install --with-deps`
  (or the nix `playwright-driver.browsers` package + `PLAYWRIGHT_BROWSERS_PATH`)
  so e2e runs headless in the sandbox.

These commands are what `build_rung` / `regress_rung` / patch_author will run
via `devbox run --` once the file exists — so the toolchain must cover them.

## 3. Map to Nix packages

devbox packages are nixpkgs names: `go`, `nodejs_22`, `python311`, `rustc`,
`cargo`, `ripgrep`, `gnumake`, `pkg-config`, … Resolve names via
`devbox search <name>` when unsure. **Non-Nix tools** (an npm-only CLI, etc.)
are NOT devbox packages: supply the runtime via devbox (`nodejs`) and install
the tool in a `shell.init_hook` (`npm i -g <tool>`), or leave it to the
project's own scripts.

## 4. Shape

```json
{
  "$schema": "https://raw.githubusercontent.com/jetify-com/devbox/<v>/.schema/devbox.schema.json",
  "packages": ["go@1.23", "nodejs@22", "pnpm@9"],
  "env": { "DEVBOX_NO_PROMPT": "true" },
  "shell": { "init_hook": ["pnpm install --frozen-lockfile || true"] }
}
```

Keep it minimal — only what build/test/e2e need. Avoid cosmetic `init_hook`
noise (it runs on every `devbox run`); reserve it for genuine setup
(dependency install, playwright browsers, env exports).

## 5. Validate before proposing

Run `devbox install` in the repo root: it must exit 0 and produce
`devbox.lock`. Then smoke the key commands: `devbox run -- <build>` and
`devbox run -- <test>` (or at least `devbox run -- <lang> --version`) to
confirm the toolchain resolves. If `devbox install` is slow on first run
(cold Nix fetch), that is expected — the persistent store (ADR-017 #1) warms
it across runs.

## 6. Scope + safety

- Write only `devbox.json` + `devbox.lock`. Do not edit source, CI, or other
  configs.
- If the repo ALREADY has a `devbox.json`, do not clobber it — propose a diff
  (add missing tools), and respect existing pins.
- The generated file is committable + human-reviewable; default to proposing
  it behind a human gate (a dev environment is consequential).

## See also
- `[[../../docs/adr/017-devbox-first-bot-toolchain]]` — the two-tier model
  this bot feeds (Tier-2 project devbox, Tier-3 = this bot).
