# A bot's toolchain is its `devbox.json`

Read it when a bot needs a binary the sandbox does not carry.

## A bot that needs tools declares them in `devbox.json`

**If a bot's steps need a binary the sandbox image does not ship, add a
`devbox.json` next to its `main.bot`.** iterion auto-installs it and puts
the resulting tools on `PATH` for every node of the run. The same applies
to a `devbox.json` at the root of the TARGET repo: iterion loads that one
too, so a bot inherits the toolchain the repo itself declares.

**On every driver, the pod backend included.** A bundle reaches a
container as a host bind mount and the kubernetes driver has none, so
there the config is not read from in-container — it is CARRIED there,
written out by the install prologue before `devbox install` runs. Worth
knowing because it was a decline until 2026-09-10, and that shape is the
one to watch for in its whole class: the feature worked on a laptop and
was inert on the driver bots actually run on, with nothing failing except
the step that needed the tool. Ceiling: the config+lock pair must stay
under 512 KiB, and a pair over it is declined by name, never installed
from a directory it never reached.

This is the supported way, and the alternatives are all worse:

- **Curling a binary in `post_create`** — unpinned, undeclared, and
  invisible to anyone reading the bot.
- **A bespoke sandbox image** (the `-sec` variant) — a CI image chain to
  maintain for every new tool, and a bot pinned to an image instead of to
  the tools it actually needs.
- **Letting the agent improvise** — the failure this rule exists for. In run
  019f8384 the deploy step needed `crane` to publish an image, the sandbox
  had no container tooling at all (no docker/podman/buildah/skopeo, `sudo`
  blocked by `no_new_privs`, no `newuidmap` for rootless BuildKit), and the
  agent spent turns discovering that, fetched a binary itself, then fell back
  to a workaround that produced a live URL and delivered nothing.

**Pin the versions and commit `devbox.lock`.** `some-tool@latest` re-resolves
at install time, so what lands in a run's sandbox can change with no commit
anywhere — a supply-chain surface, and a reproducibility hole for a bot whose
job is to ship code. The lock pins each package to an exact nixpkgs commit;
the explicit version in `devbox.json` makes the intent readable in a diff.
Generate it with `devbox install` in the bot's directory and commit both
files — the engine copies the lock alongside the config, so a locked project
installs exactly what it was authored against.

**A run that does not BUILD the target repo can decline its toolchain.**
`repo_devbox: off` on the `workflow` block skips the *target repo's*
`devbox.json` (never the bot's own) — precedence `--repo-devbox` → workflow
→ `ITERION_REPO_DEVBOX` → **on**, diagnostic C134, and the declined source is
reported on the `sandbox_devbox_provisioned` event rather than dropped in
silence. Reviewers ship with it off (`review-pr`, `revi-converse`): reading a
diff bought nothing from iterion's own 319 Nix paths / 1.8 GiB, and the cold
realise outlasted the sandbox start window often enough to kill runs. Fixers
and updaters (`branch-improve-loop`, `dep-update-guard`, `feature-dev`) keep
it **on** — they build what they change. See
[docs/dsl.md](../../dsl.md#the-target-repos-toolchain--repo_devbox).

Two things to know when writing one:

- **Non-interactive PATH is the trap.** `tool` nodes run through a
  non-interactive `bash -c` that never sources a shell profile, so a tool that
  is installed but not on `PATH` is a tool that does not exist. The engine
  prepends the devbox profile's bin dir for this reason — don't hand-roll it
  per bot.
- **Nix installs cost time.** Declare what the bot genuinely needs. A bot
  with no `devbox.json` pays nothing.

The bar for reaching past devbox (a dedicated image) is a tool that Nix does
not package, or a base layer the run needs *before* any step executes.

