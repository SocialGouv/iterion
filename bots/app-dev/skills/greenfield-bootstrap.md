---
name: greenfield-bootstrap
description: Stack-agnostic scaffold and walking-skeleton discipline for creating a NEW application in a bare workspace — stack detection, official non-interactive scaffolders, git init, commit taxonomy, brownfield detection, first-draft definition of done. Read this before writing the first file of a greenfield app.
---

# greenfield-bootstrap

You are creating an application in a workspace that may be completely
empty. The order of operations below exists because every later
guarantee (deterministic verify gate, resumability, brownfield re-runs)
keys off git history and a runnable tree — so the repo, the scaffold and
the skeleton come before any feature.

## 0. Brownfield check — ALWAYS first

Before scaffolding anything, look for project markers at the workspace
root: a package manifest (`package.json`, `pyproject.toml`, `Cargo.toml`,
`go.mod`, `Gemfile`, `composer.json`, `*.csproj`, …), a lockfile, a build
file (`Makefile`, `Taskfile.yml`, `justfile`), or a `SPEC.md` next to
source directories. **Any hit ⇒ the app exists.** Read `git log` and the
tree, then EVOLVE it slice by slice — never re-run a scaffolder over an
existing app (scaffolders overwrite configs and entry points
destructively). A `SPEC.md` alone in an otherwise bare tree is NOT
brownfield — that is the interview handoff; scaffold normally.

## 1. Detect the requested stack

In order of authority: the SPEC.md "Stack" section → the app prompt →
the operator's `stack` hint var. When none names a stack, pick the
mainstream option that fits the journeys (a content site ≠ a CLI ≠ an
API) and record the choice in an ADR — do not ask unless two options
are genuinely incompatible with the brief.

## 2. git init, then the OFFICIAL scaffolder — non-interactively

On a bare workspace, the very first mutation is:

```sh
git init -b main
```

(Never clobber an existing `.git`.) Then run the stack's OFFICIAL
scaffolder exactly as its own docs say, **with its non-interactive
flags** — a scaffolder waiting on stdin in a headless run hangs until
the timeout. Verify the flags with `--help` first if unsure. Examples of
the shape (indicative, not exhaustive — the ecosystem's docs are the
authority; check for newer flags):

| Intent            | Canonical non-interactive scaffold                                                     |
|-------------------|----------------------------------------------------------------------------------------|
| Next.js app       | `npx --yes create-next-app@latest . --yes --typescript --app --eslint --src-dir`        |
| Vite + React      | `npm create -y vite@latest . -- --template react-ts`                                    |
| Django project    | `pip install django && django-admin startproject <name> .`                              |
| FastAPI service   | `pip install fastapi uvicorn` + author `app/main.py` by hand (no official scaffolder)   |
| Express API       | `npm init -y && npm install express` + author `src/server.js` by hand                   |
| Rust CLI          | `cargo init --name <name>`                                                              |
| Go service        | `go mod init <module>` + author `cmd/<name>/main.go` by hand                            |
| Rails app         | `rails new . --skip-bundle --force`                                                     |

Rules that apply to all of them:
- **Scaffold into `.` (the workspace root)**, not a subdirectory — the
  workspace IS the app repo. If the tool refuses a non-empty dir because
  of `SPEC.md`/`.git`, use its force/overwrite flag or move the spec
  aside for the scaffold step and restore it.
- If the scaffolder generated its own `.git` (some do), keep ONE repo at
  the root: remove the nested one, keep root history.
- Make sure a sensible `.gitignore` exists (scaffolders usually write
  one; author it yourself when scaffolding by hand — dependency dirs,
  build output, env files).
- Commit immediately: `chore(scaffold): <stack> baseline via <tool>`.
  This commit is deliberately unmodified scaffolder output — reviewers
  skip it, and later passes can diff "our work" against it.

## 3. Walking skeleton before features

Slice 1 is the smallest END-TO-END path that proves the app runs:

- one visible surface (a rendered page, a `GET /api/healthz` returning
  200, a CLI command printing something real);
- one smoke test asserting it (the stack's native test runner);
- a run command a human can type (`npm run dev`, `python manage.py
  runserver`, `cargo run`) — verified by actually running it;
- committed as `feat(skeleton): <the visible thing>` (+ `test(smoke):`
  if separate).

No feature work before the skeleton builds, runs and its smoke test
passes. When the skeleton is in, add a top-level README (install / run /
test commands) — `docs(readme): run and test commands`.

## 4. Commit taxonomy for the rest of the run

`feat(scope):` one coherent feature slice · `test(scope):` tests beyond
a slice's own · `fix(scope):` red-gate fixes · `docs(...)` README/ADRs ·
`chore(...)` config/deps. Stage with `git add -A` before EVERY commit
(greenfield adds files constantly; an unstaged new file is invisible to
diffs and to the verify gate's drift check).

## 5. First-draft definition of done

Report the app complete ONLY when all four hold:

1. **Builds** — the stack's build/compile command exits 0.
2. **Runs** — the app starts and the skeleton surface responds (curl the
   page/endpoint, or execute the CLI) — a command you actually ran.
3. **Smoke passes** — the test suite (at minimum the smoke test) is
   green.
4. **README documents it** — install, run, test; plus the URL or command
   where a human sees the app working.

The `how_to_run` you report must be the literal commands from (2)–(4) —
the operator at the draft-review gate pastes them verbatim. If any of
the four is not true, the draft is not done; say what remains instead.
