import { describe, expect, it } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import {
  cardHasAllTags,
  cardTags,
  collectTagVocabulary,
  faceTags,
} from "./cardTags";

function card(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "c1",
    kind: "run",
    column_id: "opened",
    title: "t",
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...partial,
  };
}

describe("cardTags", () => {
  it("unions issue labels with content-derived tags", () => {
    const tags = cardTags(
      card({
        labels: ["urgent", "video"],
        entry_input: {
          character: "Boudicca",
          episode_no: "1",
          episode_total: "5",
          episode_title: "Le Fouet",
          pipeline_kind: "shorts",
        },
      }),
    );
    expect(tags).toContain("urgent");
    expect(tags).toContain("video");
    expect(tags).toContain("Boudicca");
    expect(tags).toContain("ÉP 1/5");
    expect(tags).toContain("shorts");
    // episode_title is for the card title, not a tag (too long / unique).
    expect(tags).not.toContain("Le Fouet");
  });

  it("dedupes case-insensitively and skips paths / long values", () => {
    const tags = cardTags(
      card({
        labels: ["Boudicca", "path/to/x"],
        entry_input: {
          character: "boudicca",
          family_id: "assets/humanoid/foo", // path-ish → dropped
        },
      }),
    );
    expect(tags.filter((t) => t.toLowerCase() === "boudicca")).toHaveLength(1);
    expect(tags.some((t) => t.includes("/"))).toBe(false);
  });

  it("cardHasAllTags is AND over the full vocabulary", () => {
    const c = card({
      labels: ["a"],
      entry_input: { character: "Boudicca", episode_no: "2", episode_total: "5" },
    });
    expect(cardHasAllTags(c, new Set(["a", "Boudicca"]))).toBe(true);
    expect(cardHasAllTags(c, new Set(["a", "missing"]))).toBe(false);
    expect(cardHasAllTags(c, new Set(["ÉP 2/5"]))).toBe(true);
  });

  it("faceTags caps chips and reports overflow", () => {
    const c = card({
      labels: ["a", "b", "c", "d", "e"],
    });
    const { shown, more } = faceTags(c, 3);
    expect(shown).toEqual(["a", "b", "c"]);
    expect(more).toBe(2);
  });

  it("collectTagVocabulary sorts the board-wide set", () => {
    const vocab = collectTagVocabulary([
      card({ id: "1", labels: ["zeta"], entry_input: { character: "Ada" } }),
      card({ id: "2", labels: ["alpha"] }),
    ]);
    expect(vocab).toEqual(["Ada", "alpha", "zeta"]);
  });
});
