import type { PipelineBoardCard } from "@/api/pipelineBoards";

// findFollowCard re-locates the operator's selected card in a freshly polled
// board. A pipeline card's id changes when a not-yet-run task ("task:<issue>")
// starts and becomes a run ("run:<id>"), so a plain id match would drop the
// selection mid-transition. We fall back to the stable issue id, then the run
// id, so the details sidebar keeps tracking the same pipeline across those
// hops. Returns null when the card is genuinely gone from the projection.
export function findFollowCard(
  cards: PipelineBoardCard[],
  anchor: PipelineBoardCard,
): PipelineBoardCard | null {
  const byId = cards.find((c) => c.id === anchor.id);
  if (byId) return byId;
  if (anchor.issue_id) {
    const m = cards.find((c) => c.issue_id === anchor.issue_id);
    if (m) return m;
  }
  if (anchor.run_id) {
    const m = cards.find((c) => c.run_id === anchor.run_id);
    if (m) return m;
  }
  return null;
}
