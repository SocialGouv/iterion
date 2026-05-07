# Devbox-ready devcontainer for `sandbox: auto`

This directory shows how to author a `.devcontainer/devcontainer.json`
that extends the iterion default sandbox image with workflow-specific
toolchains via a workspace `devbox.json`.

Drop these two files at your repo root:

```
.devcontainer/devcontainer.json   # this template
devbox.json                       # your workflow's toolchain
```

When a workflow with `sandbox: auto` runs, iterion reads the
devcontainer above, pulls `iterion-sandbox-slim:<iterion-version>`, and
runs `devbox install` once during `postCreateCommand` to materialise
the packages declared in `devbox.json`. Subsequent commands invoked
through tool nodes inherit the devbox environment.

## What you get for free in iterion-sandbox-slim

- `git`, `curl`, `bash`, `tini`, `jq`
- Node.js 22 (system install, used by `claude` / `codex` CLIs)
- `devbox` + Nix (single-user, ready for `devbox shell` / `devbox run`)

## What you add via devbox.json

Anything the iterion image doesn't ship system-wide. The example
`devbox.json` here pins Go 1.25, Python 3.12, and jq through Nix —
identical to the way the iterion repo itself manages its own
toolchain.

## Without this file

Workflows with `sandbox: auto` and no `.devcontainer/` will still run:
iterion falls back to `iterion-sandbox-slim:<iterion-version>` and
trusts the workflow to install whatever it needs at runtime via tool
nodes. The cost is not having a project-pinned toolchain — for
reproducible runs prefer the explicit devcontainer above.
