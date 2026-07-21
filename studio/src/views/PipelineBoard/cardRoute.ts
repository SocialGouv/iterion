import type { PipelineBoardCard } from "@/api/pipelineBoards";

// URL key for a pipeline card. Prefer issue_id (stable across task→run
// transition), then run_id, then the projection card id.
export type CardRouteKey =
  | { kind: "issue"; id: string }
  | { kind: "run"; id: string }
  | { kind: "id"; id: string };

export function cardRouteKey(card: PipelineBoardCard): CardRouteKey {
  if (card.issue_id) return { kind: "issue", id: card.issue_id };
  if (card.run_id) return { kind: "run", id: card.run_id };
  return { kind: "id", id: card.id };
}

/** Path segment pair for wouter: /pipelines/cards/:kind/:id */
export function cardRoutePath(card: PipelineBoardCard): string {
  const key = cardRouteKey(card);
  return `/pipelines/cards/${key.kind}/${encodeURIComponent(key.id)}`;
}

export function parseCardRoute(
  kind: string | undefined,
  id: string | undefined,
): CardRouteKey | null {
  if (!kind || !id) return null;
  const decoded = decodeURIComponent(id);
  if (kind === "issue" || kind === "run" || kind === "id") {
    return { kind, id: decoded };
  }
  return null;
}

export function findCardByRouteKey(
  cards: PipelineBoardCard[],
  key: CardRouteKey,
): PipelineBoardCard | null {
  switch (key.kind) {
    case "issue":
      return cards.find((c) => c.issue_id === key.id) ?? null;
    case "run":
      return (
        cards.find((c) => c.run_id === key.id) ??
        cards.find((c) => c.id === `run:${key.id}`) ??
        null
      );
    case "id":
      return cards.find((c) => c.id === key.id) ?? null;
  }
}
