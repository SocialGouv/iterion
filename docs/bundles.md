[← Documentation index](README.md)

# Bundles — `.botz` packaged workflows

A **bundle** is a tar.gz that ships a workflow (`bot.iter`) alongside
the resources it depends on — Claude Code skills, reusable prompts,
default attachments, a manifest. The result is a single `.botz` file
you can email, commit, or drop into S3, and that any `iterion` install
can run with one command.

```bash
iterion bundle init  my-bot         # scaffold
$EDITOR my-bot/bot.iter             # write your workflow
iterion bundle pack  my-bot         # → my-bot.botz
iterion run          my-bot.botz    # run it
```

## Why bundles

A plain `.iter` is one file. As soon as the workflow needs adjacent
resources — a project-local Claude Code skill, a reviewer prompt,
sample input PDFs — those files have to live on every machine the
workflow runs on. Bundles solve that: everything ships together, with
a stable content hash that lets two machines extracting the same
bundle reuse the same cache slot.

Bundles are also the unit of distribution we expect for shared
workflows (templates, examples, organisation-internal recipes).

## Quick start

```bash
# 1. Scaffold a layout under ./my-bot.
iterion bundle init my-bot

# 2. Edit bot.iter, drop skills/prompts/attachments as needed.
$EDITOR my-bot/bot.iter
echo "# my skill" > my-bot/skills/probe.md
echo "Hello {{vars.topic}}" > my-bot/prompts/helper.md

# 3. Build the deterministic archive.
iterion bundle pack my-bot
#  → my-bot.botz   (next to the source dir)

# 4. Run it like any workflow file.
iterion run my-bot.botz
iterion run my-bot.botz --preset quick    # named preset from bot.iter
```

## Layout

```
my-bot/
├── bot.iter           # required (or bot.bot — same semantics)
├── manifest.yaml      # optional
├── README.md          # optional, for human readers
├── skills/            # optional — Claude Code skills
│   └── probe.md
├── prompts/           # optional — reusable .md prompts
│   └── helper.md
└── attachments/       # optional — default values for `attachments:` block
    └── logo.png
```

| Entry             | Purpose |
| ----------------- | ------- |
| `bot.iter`        | The workflow source. Must live at the bundle root. |
| `manifest.yaml`   | Bundle metadata (name, version, schema_version, optional `attachments:` map). Optional. |
| `skills/`         | Claude Code skills. Mirrored into `<workDir>/.claude/skills/` at run time. Workspace files always win on collision (warn-logged). |
| `prompts/`        | Reusable `.md` prompts. Each file is auto-registered with name equal to the filename stem — `prompts/helper.md` makes `system: helper` resolvable from `bot.iter`. Workflow-declared prompts always win on collision. |
| `attachments/`    | Default binary inputs the manifest can map to declared `attachments:` entries. Runtime uploads (Launch modal, cloud) override these. |

## Manifest schema

```yaml
name: my-bot              # human-friendly identifier (display only)
version: 0.1.0            # free-form, semver recommended
description: One-liner.
author: Your Name <you@example.com>
schema_version: 1         # required; iterion refuses unknown versions

# Optional: map workflow attachment names → files inside attachments/
attachments:
  logo: branding/logo.png
  spec: docs/spec.pdf

# Reserved for future minor extensions (additive). Unknown keys are
# tolerated under `compat:` so newer bundles don't break older iterion.
compat:
  some-future-key: …
```

The current schema version is **1**. Bundles that omit `schema_version`
are treated as v1. iterion refuses any other value with an explicit
upgrade hint.

## Determinism

`iterion bundle pack` produces a **reproducible** archive:

- entries sorted alphabetically;
- timestamps zeroed (tar `ModTime`, gzip `ModTime`);
- ownership stripped (uid/gid 0, uname/gname empty);
- modes normalised (`0o644` for files, `0o755` for dirs);
- gzip OS byte set to `unknown`, gzip `Name`/`Comment` stripped;
- USTAR format pinned (`tar.FormatUSTAR`).

```bash
iterion bundle pack my-bot -o a.botz
iterion bundle pack my-bot -o b.botz
sha256sum a.botz b.botz
# 03551558…  a.botz
# 03551558…  b.botz   ← identical
```

This matters because the **uncompressed tar SHA-256** is the cache key
the consumer side uses to look up the extraction slot at
`~/.cache/iterion/bundles/<hash16>/`. Two machines packing the same
source produce the same hash → cache hits become trivially shareable
(e.g. via a CDN that serves the archive but lets each machine extract
locally).

## Resource resolution at run time

When `iterion run my.botz` (or a `.bot` directory bundle) executes:

1. **Skills** in `skills/` are copied into `<workDir>/.claude/skills/`
   (workspace files take precedence on name collision — shadowed
   bundle skills are warn-logged).
2. **Prompts** in `prompts/*.md` are merged into the AST `prompts:`
   table **before** static validation runs, so node-level
   `system:`/`user:` references against bundle filenames type-check.
   Workflow-declared prompts always win on collision.
3. **Attachments** listed in `manifest.yaml`'s `attachments:` map are
   promoted via `store.WriteAttachment` **before** the host's
   attachment-promote callback, so a runtime upload of the same name
   overrides the bundle default.
4. **Sandbox**: when active, the bundle directory is bind-mounted
   read-only at `/run/iterion/bundle` (parallel to
   `/run/iterion/attachments`). Resources stay reachable from inside
   the container even though the cache slot lives outside the
   workspace mount.

## Cache & resume

Bundles are extracted once, content-addressed by hash. The slot is
marked ready with a `.ready` sentinel for atomic concurrent extraction
and carries a `bundle.lock` recording the full hash + original archive
path.

- **Cache hit**: `iterion run my.botz` reuses
  `~/.cache/iterion/bundles/<hash>/` immediately.
- **Cache miss / GC**: iterion re-extracts from `BundlePath` recorded
  on the run.
- **Cache + source both gone**: resume fails with a clear hint
  pointing at the archive to re-supply.

`iterion resume --run-id <id>` re-opens the bundle from the run's
persisted `BundlePath` automatically — the user doesn't re-type
`--preset` or paths, the engine pulls them from `run.json`.

## CLI reference

```
iterion bundle init <dir>                Scaffold a bundle source layout.
iterion bundle pack <dir> [-o file]      Build a deterministic .botz from a dir.
                       [--force]         Overwrite the output if it exists.
iterion validate <bundle.botz|dir>       Validate a bundle and its workflow.
iterion run <bundle.botz> [--preset]     Run a workflow from a bundle.
iterion resume --run-id <id>             Resume a bundle-launched run.
```

## Files the packer skips

The packer ignores patterns that are never useful inside a bundle and
that would defeat determinism:

- `.git/` — version control noise.
- `.iterion/` — local run store of past iterion runs.
- `*.botz` — prior builds (avoids accidental nested packaging).
- `.DS_Store`, `*.swp`, `*~` — OS/editor scratch.

Symlinks, devices, sockets, and other non-regular entries are
**rejected** at pack time with a clear error.

## Troubleshooting

**`bundle: re-extract <path> required (cache miss; original archive absent)`**
The cache slot was purged and `BundlePath` no longer resolves on disk.
Re-supply the archive (or rebuild from source with `iterion bundle pack`).

**`bundle: schema_version N not supported by this iterion build`**
The bundle was produced by a newer iterion. Either upgrade your iterion
install or downgrade the bundle (set `schema_version: 1` in
`manifest.yaml`).

**`bundle skill "X" shadowed by existing workspace entry`**
A skill with the same name already exists at `<workDir>/.claude/skills/`.
The workspace copy wins — rename either to disambiguate.

**`bundle/pack: symlinks not allowed`**
The packer refuses symlinks to keep the archive content-stable. Move
the target into the bundle tree (or copy it explicitly), or use the
filesystem outside the bundle if it's a host-specific resource.
