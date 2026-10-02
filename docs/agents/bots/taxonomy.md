# Category & tags — the six-verb navigation spine

Read it when touching a manifest's `category:` / `tags:`, adding a tag
to the vocabulary, or wiring a new catalog surface to the spine
([the vocabulary](../../pkg/bundle/vocab.go) is the source; the closed
set is bundlelint's soft diagnostic, the fleet gate's hard one).

++ b/docs/agents/bot-authoring.md
## Category & tags — the six-verb navigation spine

Every shipped catalog bot declares `category:` and `tags:` in its
manifest. They are **advisory display metadata** (like `when_to_use`):
the runtime never branches on them, discovery just carries them, and no
surface may hide a bot for what they say.

**`category` is a CLOSED, versioned set** — one of six slugs
([pkg/bundle/vocab.go](../../pkg/bundle/vocab.go), mirrored by
`studio/src/lib/botTaxonomy.ts` under a Go parity test):

| slug | reads as | fleet examples |
|---|---|---|
| `build` | ship new capability | feature-dev, app-dev |
| `verify` | judge the code, touch nothing | review-pr, sec-audit-* |
| `harden` | strengthen what exists | branch-improve-loop, secured-renovacy |
| `document` | align words with code | docs-refresh, adr-cartograph |
| `operate` | run the delivery machinery | feed-watch, review-env |
| `steer` | judge the direction, converse | whats-next, arbitrate |

The order is the lifecycle (create → judge → strengthen → explain → run →
decide) and is part of the contract — every grouped surface renders it in
this order. Adding a seventh slug is a **product decision** documented
here and in the vocabulary file, never an opportunistic manifest edit.
An unknown value is never rejected or rewritten: the bot lands in the
visible **Uncategorized** group, last, and bundlelint emits C270 (a
warning) naming the known slugs. Absence is different: a SHIPPED bot
without a category is a catalog regression —
`bots/catalog_taxonomy_test.go` fails the build on it.

**`tags` are an OPEN, governed vocabulary** — orthogonal facets
(domain, safety, modality: `security`, `deps`, `read-only`,
`ships-code`, `conversational`, …). The seed lives next to the
categories; `iterion bots list --tag a --tag b` ANDs them, the studio
chips narrow the gallery, and `?tag=…` on `/bots` is a shareable view.
Governance is the label-vocabulary lesson: **reuse before inventing**.
An unseeded tag stays declared (never a load error) and emits C271
(warning) — but the shipped-catalog consistency gate
(`bots/bundle_consistency_test.go`) holds the fleet to zero warnings, so
a new tag on a catalog bot means seeding `vocab.go` in the same change.
Third-party bundles feel only the soft lint.

The safety pair carries a WRITTEN boundary — the tag answers "what will
this run leave behind?". For a bot whose run targets a repository,
EXACTLY ONE of the pair applies (carrying both is a contradiction the
fleet gate refuses): `ships-code` = the run COMMITS into the target
repo's history (source, docs, wiki, config, versioned state — anything
`git log` shows: golden-master is ships-code even though its product is
a test net, because it commits; the two supply-shields are read-only
even though they read dependencies deeply, because a finding goes to
the board and no commit exists); `read-only` = the run writes nothing
anywhere. A conversational or board-only bot (Nexie, Copi, Triagy)
targets no repository and legitimately carries neither.

Two things to know:

- **Strict decoding makes the two keys load-breaking for older builds**
  (house policy since `launch:`/`chat:`/`retry:`): a runner older than
  the feature refuses the whole bundle at admission rather than dropping
  the fields. Fleet and binaries upgrade together; update a pinned
  runner before pulling manifests that declare the keys.
- **Marketplace fields are a different vocabulary**: when prefilling a
  marketplace submission from these manifest keys one day, map them to
  NEW marketplace entry fields — the existing `Entry.Categories`
  (plugin contribution kinds) and submit-time `Entry.Tags` mean
  something else, and overwriting them corrupts plugin grouping.

