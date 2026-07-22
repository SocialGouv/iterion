# devbox-setup (Devy) — status: v1, dogfood-validated end-to-end (detect → generate → verify → commit → deliver)

Devy authors a pinned `devbox.json` for a repo so its build/test/e2e run in a
reproducible toolchain (ADR-017 Tier-3). This bundle ships the **manifest**,
the **playbook skill** (`skills/devbox-setup.md` — the substance: detect →
map to Nix → shape → validate → scope), and the **`main.bot`** (linear
detect → generate → verify → commit → done; `iterion validate` OK, 6 nodes).

**Dogfood (2026-06-08, minimal Go repo) — green end-to-end.** `detect_stack`
identified Go 1.22, `generate_devbox` wrote a correct pinned `devbox.json`
(`{"packages":["go@1.22"]}`), `verify_devbox` ran `devbox install` in the
sandbox to `ok:true`, `commit_devbox` committed it, and the run
fast-forwarded the target → the `devbox.json` was delivered. Three bugs were
found + fixed along the way: (1) the `model:` field needs `${ENV}` not
`{{vars}}` (a `{{vars}}` value was passed literally to the CLI); (2) `$HOME`
is force-set to the host home in the sandbox, but the image's nix/devbox live
under the container user's home (`/home/devbox`) → run devbox with
`HOME = the real user home` (`pwd.getpwuid(os.getuid()).pw_dir`) or it can't
resolve its store (the same fix applies to Seki's Tier-2 build/regress
rungs); (3) without a commit the generated file was discarded with the
worktree, so a `commit_devbox` node was added. Set
`ITERION_SANDBOX_PERSIST_NIX=1` to reuse the `/nix` store across runs
(ADR-017 #1) so `devbox install` warms.

## Intended workflow (build spec)

```
detect_stack  (agent, claude_code, read-only: read_file/glob/grep/bash)
  → reads manifests (go.mod, package.json, pyproject, …) + build/test/e2e
    commands; emits {languages, runtimes, build_cmd, test_cmd, e2e, pins}
generate_devbox (agent, claude_code, tools incl. write_file)
  → writes /workspace/devbox.json per skills/devbox-setup.md (pinned, minimal)
verify_devbox  (tool, deterministic)
  → `cd <workspace> && devbox install` (must exit 0 + produce devbox.lock);
    degrade-with-report on fail
done
```

- **sandbox**: none declared (ADR-082 — the platform sandboxes by
  default, and the default slim image ships everything this bot needs:
  node + devbox/nix + the claude CLI).
- **worktree: auto** (it writes a file → isolate + gate before it lands).
- **vars**: `workspace_dir`, `apply_mode` (propose|apply), `devbox_model`.
- **idempotent**: if `/workspace/devbox.json` exists, propose a diff (add
  missing tools), never clobber existing pins (see skill §6).

## v1 scope + next
v1 is the linear flow above (detect → generate → verify → done); the
worktree + PR review is the gate. Next enhancements: an in-bot `human`
approve_devbox gate + an `apply_mode` (propose | apply), and a real dogfood
run (a target repo + a stable network for the cold `devbox install`) to
confirm the generated devbox.json installs.
