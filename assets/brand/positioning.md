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
| `docs/scripts/og-card.html` | 1 | the OpenGraph image — **regenerate `docs/public/og.png` with `task brand:og` after editing**, and `task brand:og:check` verifies the committed PNG was rendered from THIS card (a recorded source hash, not a pixel compare — the render depends on host fonts) |
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

## Where a SHORT form is used instead — deliberately, and unguarded

The list above is **not the whole class**, and saying otherwise is how a guard
starts lying. These surfaces carry a short descriptor because their format
refuses a sentence: a `.desktop` `Comment=` is a one-liner, a Cobra `Short`
sits on one terminal line, and a Homebrew `desc` audit rejects both a trailing
period and a leading article. So the short form is the same sentence without
its final period:

```text
Build, run and orchestrate agentic AI workflows
```

| Surface | What it is | Carries the short form |
|---|---|---|
| `cmd/iterion/main.go` | the Cobra `Short`/`Long` — `iterion --help` | ✅ |
| `charts/iterion/Chart.yaml` | the `description:` ArtifactHub and `helm show chart` display | ✅ |
| `Formula/iterion.rb`, `Cask/iterion-desktop.rb` | Homebrew `desc` | ❌ still "Workflow orchestration engine" |
| `build/linux/iterion.desktop` | the Linux launcher `Comment=` | ❌ |
| `build/windows/info.json`, `cmd/iterion-desktop/wails.json` | Windows/Wails bundle metadata | ❌ |
| `sdks/typescript/README.md`, `marketplace.json` | package descriptions | ❌ |
| `studio/docs/visual-identity.md` | the studio's visual-identity brief | ❌ |

None of these is guarded: they are release-packaging inputs whose wording is
constrained by a third party's linter, and pinning them to a byte would turn a
packaging-rule change into a red build on an unrelated pull request. The
Homebrew tap files are rewritten by a bot on every release
(`chore(brew): update tap to vX.Y.Z`), so editing them by hand here would race
it.

The ❌ rows are **not done**, deliberately and visibly: this table is the
inventory to walk when someone decides to unify them, and an inventory that
claimed they were already done would be the lie this file exists to prevent.
