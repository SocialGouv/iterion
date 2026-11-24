# ADR-140: Discovery claims what the bundle package claims, and a nested bundle is nothing

**Status:** Accepted (2026-10-27, the #2537 stream; the operator's arbitration the same day — slices A and C delivered together, the namespace decision is "nothing")
**Relates to:** [103-run-addressed-by-id-across-teams.md](103-run-addressed-by-id-across-teams.md) (no relation in tenancy; cited as the one-resource discipline this stream follows), the promotion 9280244432 that made pkg/bundle "the sole definition of what a bundle is" — the discovery walk was its last defector — and the corner recorded in docs/agents/engine/bot-catalog-regeneration.md

## Context

Discovery and the bundle package disagreed about what a directory is. The walk kept a private two-file test — `manifest.yaml` (that exact spelling) AND `main.bot`, by existence alone — while the package's markers read an iterion-SHAPE verdict over BOTH spellings (`manifest.yaml`, `manifest.yml`) plus a `skills/` directory, refusing another tool's manifest by its keys. Three consequences, all reachable in one move through the localize path that lands `plugins/<dep>/` under a consumer bundle:

- a consumer whose manifest is spelled `manifest.yml` was not claimed: the walk descended it, the localized copy leaked into the routing namespace as a TOP-LEVEL bot, and the consumer itself was demoted to a loose `main.bot` with no manifest identity — two such consumers collide;
- a consumer that lost its `main.bot` was not claimed either: the same leak, and the consumer vanished from the catalogue in silence;
- after either, `RegenerateWhatsNextCatalog` refuses (a shadowed bundle is blocking), so every catalogue refresh of the workspace fails until manual surgery.

The localize path was the only production writer of that shape; its refusal (shipped before this ADR, as the same stream's first slice) closes the reachable path. This ADR retires the disagreement itself.

## Decision

1. **The claim verdict lives in the bundle package.** `bundle.DirClaim(dir)` answers (claimed, openable): claimed = the directory carries an iterion manifest — either spelling, judged by the SHAPE test the markers already use (`manifestVerdict`) — or a `skills/` directory; openable = `main.bot` sits beside the claim. Discovery replaces its private two-file test with this verdict; so does the localize path. A directory is a bundle on every surface or on none.
2. **A claimed directory that does not open is a loud skipped source, never a descent.** The walk records a diagnostic (a distinct kind, classified non-blocking for the catalogue: nothing was listed under it, so no row is lost) and skips the subtree. Letting the walk in is what leaked the copy; a bundle that exists but has no entrypoint must not have its privacy walked through. The localize refusal is the gate that keeps NEW wedges out; the diagnostic surfaces the ones already on disk.
3. **A bundle nested under a bundle is called NOTHING.** It is a private subtree of its consumer, invisible to discovery while the consumer is claimed. The dependency's public name stays the lock name, owned by the `.botz` materialization tier. No `consumer/dep` spelling exists and none may be introduced casually: bot names flow into lock keys (no `/`), `bot://` URIs (which reject `/`), board attribution, run documents, bot-scoped memory and secret bindings — a slash in `Entry.Name` would break every one of them at once. An operator who wants the nested copy launchable standalone publishes it; the copy itself is not addressable.

## What is not promised

- **Repair of already-wedged workspaces.** The refusal stops new wedges; the diagnostic names existing ones. Fixing a wedged workspace is still manual (restore the spelling, restore `main.bot`, or remove the landed directory).
- **Trees copied by hand.** The localize path was the production writer; a hand-copied `plugins/<dep>/` under an unclaimed consumer is not prevented by the refusal — it is surfaced by the same diagnostics and wedges the same regeneration.
- **A catalogue identity for nested bundles.** Decision 3 is a permanent "no": the namespace stays flat.

## Behavior changes this lands (documented breaks, no `!`)

- A `manifest.yml` consumer is a bundle now: it lists under its own name with manifest identity (it used to demote to a loose `main`), and the localize accepts it (it used to be refused — the refusal was the interim slice of this same stream).
- A directory with an iterion-shaped manifest and no `main.bot` (either spelling), or with only a `skills/` directory, is skipped loudly instead of descended: loose `.bot` files under it no longer surface as top-level bots.
- A foreign manifest (Helm, compose, CI — by shape) beside a `main.bot` marks nothing, as for `DirForMainBot`: the walk descends and the `main.bot` reads as the loose bot it is, instead of a bundle whose manifest belonged to another tool.
