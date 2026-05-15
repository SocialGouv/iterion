# my-bot

A packaged iterion workflow (`.botz` bundle).

## Layout

- `bot.iter` — the workflow source. Replace with your own.
- `manifest.yaml` — bundle metadata (name, version, schema_version).
- `skills/` — Claude Code skills shipped with the bundle. Files here
  are mirrored into `<workDir>/.claude/skills/` at run time.
- `prompts/` — reusable `.md` prompts. The filename stem becomes the
  prompt name (`prompts/helper.md` → `system: helper`).
- `attachments/` — default binary inputs (referenced from
  `manifest.yaml`'s `attachments:` map).

## Build

```bash
iterion bundle pack .
```

Produces `../my-bot.botz` (deterministic — same content → same bytes).

## Run

```bash
iterion run ../my-bot.botz
iterion run ../my-bot.botz --preset quick   # apply a named preset
```

See [docs/bundles.md](https://github.com/SocialGouv/iterion/blob/main/docs/bundles.md)
for the full format reference.
