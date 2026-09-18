# Positioning — what iterion says it is, in one sentence

Two lines answer two different questions, and a public surface needs both:

- the **definition** answers *what does it do* — it is the same sentence
  everywhere, to the byte;
- the **category line** answers *what kind of thing is it* — it is a metaphor,
  and each surface is free to phrase it in its own voice (the cloud home's hero
  is written, the chart README is terse).

Only the definition is guarded. `scripts/brand/positioning-check.sh` reads the
sentence below and fails if any listed surface no longer carries it verbatim;
`task brand:positioning` runs it, and `task check` runs that. Change the wording
here, run the check, and fix every surface it names — that is the whole point of
the file existing.

## definition

```text
Build, run and orchestrate agentic AI workflows.
```

## category line

The reference phrasing. Surfaces may adapt it; they may not drop the definition
above for it.

```text
The control plane for AI agents — apps have Linux, the cloud has Kubernetes.
```

## Where the definition is displayed

The guarded list, and why each one is on it:

| Surface | Why it carries the definition |
|---|---|
| `README.md` | the repo's front page |
| `docs/index.md` | the docs hero |
| `docs/.vitepress/config.ts` | the docs site `description` + the og/twitter cards |
| `docs/scripts/og-card.html` | the OpenGraph image — **regenerate `docs/public/og.png` with `task brand:og` after editing** |
| `studio/index.html` | the served HTML of iterion.cloud: what a crawler and a link preview read |
| `studio/public/manifest.json` | the installed-app description |
| `studio/src/views/CloudHome/index.tsx` | the iterion.cloud home |
| `charts/iterion/README.md` | the Helm chart's front page |

The GitHub repository description is the ninth surface and lives outside the
tree; it is set by hand and cannot be guarded:

```sh
gh repo edit SocialGouv/iterion --description "$(…the definition…) …category line…"
```
