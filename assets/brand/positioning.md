# Positioning — what iterion says it is, in one sentence

Two lines answer two different questions, and a public surface needs both:

- the **definition** answers *what does it do* — it is the same sentence
  everywhere, to the byte;
- the **category line** answers *what kind of thing is it* — it is a metaphor,
  and each surface is free to phrase it in its own voice (the cloud home's hero
  is written, the chart README is terse).

Only the definition is guarded. `scripts/brand/positioning-check.sh` reads the
sentence below and fails when a listed surface stops carrying it the expected
number of times; `task brand:positioning` runs it, `task check` runs that, and
the CI `brand` job runs it on every pull request. Change the wording here, run
the check, and fix every surface it names — that is the whole point of the file
existing.

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

The guarded list, and how many times each surface must carry the sentence. The
count matters as much as the presence: two of these carry it three times over —
a description plus an OpenGraph and a Twitter card — and a mere presence test
stays green after both social cards are blanked, which is exactly the
link-preview regression the guard exists to catch.

| Surface | × | Why it carries the definition |
|---|---|---|
| `README.md` | 1 | the repo's front page |
| `charts/iterion/README.md` | 1 | the Helm chart's front page |
| `docs/cloud-overview.md` | 1 | the cloud product page in the docs |
| `docs/index.md` | 1 | the docs hero |
| `docs/scripts/og-card.html` | 1 | the OpenGraph image — **regenerate `docs/public/og.png` with `task brand:og` after editing**, and `task brand:og:check` checks both that the PNG is a well-formed 1200×630 file and that its recorded source hash still matches this card — not a pixel compare, because the render depends on host fonts |
| `docs/.vitepress/config.ts` | 3 | the docs site `description` + the og and twitter cards |
| `studio/index.html` | 3 | the served HTML of iterion.cloud — what a crawler and a link preview read — + its og and twitter cards |
| `studio/public/manifest.json` | 1 | the installed-app description |
| `studio/src/views/CloudHome/index.tsx` | 1 | the iterion.cloud home |

**The count is a spelling check, not a rendering check — on every surface.**
It cannot tell a rendered hero from a comment: replacing the hero while parking
the sentence in a `//` line keeps the count at one, and blanking both social
cards of `studio/index.html` while adding the sentence in an HTML comment keeps
it at three. Measured, in both shapes.

So it holds a real invariant — the sentence is present, the expected number of
times — and NOT the one a reader cares about. For the one surface where the
divergence is most plausible and cheapest to close, a React component,
`studio/src/views/CloudHome/definition.test.tsx` asserts the sentence in the
text the browser renders, which has no spelling to enumerate. The remaining
surfaces have no equivalent bench, and this paragraph is where that is written
down rather than assumed away.

The **GitHub repository description** lives outside the tree and cannot be
guarded by a script. It is set by hand and read back:

```sh
gh repo edit SocialGouv/iterion --description "Build, run and orchestrate agentic AI workflows. The control plane for AI agents — apps have Linux, the cloud has Kubernetes."
gh api repos/SocialGouv/iterion --jq .description   # read it back
```

## The SHORT form — guarded too

Some surfaces cannot take a sentence: a Homebrew `desc` audit rejects both a
trailing period and a leading article, a `.desktop` `Comment=` is a one-liner,
a Cobra `Short` sits on one terminal line. They carry the same sentence without
its final period — **derived** by the guard (`${definition%.}`), not written
down a second time, because a second literal is a second thing to keep in step:

```text
Build, run and orchestrate agentic AI workflows
```

| Surface | What it is |
|---|---|
| `CLAUDE.md` | the opening line every agent reads on every call |
| `Cask/iterion-desktop.rb` | Homebrew cask `desc` |
| `Formula/iterion.rb` | Homebrew formula `desc` |
| `build/linux/iterion.desktop` | the Linux launcher `Comment=` |
| `build/windows/info.json` | the Windows bundle `Comments` |
| `charts/iterion/Chart.yaml` | the `description:` ArtifactHub and `helm show chart` display |
| `cmd/iterion-desktop/wails.json` | the Wails bundle `comments` |
| `cmd/iterion/main.go` | the Cobra `Short`/`Long` — `iterion --help` |

Checked by PRESENCE, not by count: each carries it once by construction, and
several embed it mid-phrase.

**Editing the Homebrew files by hand is safe**, contrary to what this file said
before: `scripts/update-brew-tap.sh` rewrites only `version "…"` and
`sha256 "…"` lines — every other line is printed through untouched. That was an
assumption, corrected by reading the awk.

**Prose that describes the product in its own words** —
`sdks/typescript/README.md`, `marketplace.json`,
`studio/docs/visual-identity.md`, `bots/copilot/skills/iterion-concepts.md` —
carries the same idea in a full sentence and is deliberately NOT pinned to a
byte: freezing prose is how a guard starts blocking legitimate edits.

**The inventory was wrong once, which is why it is a guard now.** Written by
hand it listed seven surfaces; the first class grep after unifying them found
nine, including `CLAUDE.md`'s opening line — the most-read descriptor in the
repository.
