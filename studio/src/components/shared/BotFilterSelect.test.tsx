import { describe, expect, it } from "vitest";

import type { BotEntry } from "@/api/bots";

import { botFilterLabel, botFilterOptions } from "./BotFilterSelect";

const catalog: BotEntry[] = [
  {
    name: "pipeline-board-demo",
    display_name: "Episode Factory",
    icon: "🎬",
    path: "/x/bots/pipeline-board-demo",
  },
  {
    name: "whats-next",
    display_name: "Nexie",
    path: "/x/bots/whats-next",
  },
  {
    // Loose .bot file: no manifest, no display_name → stays raw.
    name: "adhoc-script",
    path: "/x/bots/adhoc-script.bot",
  },
];

describe("botFilterLabel", () => {
  it("renders the persona (manifest icon + display_name) for a catalog bot", () => {
    expect(botFilterLabel("pipeline-board-demo", catalog)).toBe(
      "🎬 Episode Factory",
    );
  });

  it("falls back to the built-in persona emoji when no manifest icon is set", () => {
    expect(botFilterLabel("whats-next", catalog)).toBe("🧭 Nexie");
  });

  it("resolves snake_case workflow-name fallbacks to the kebab catalog entry", () => {
    expect(botFilterLabel("pipeline_board_demo", catalog)).toBe(
      "🎬 Episode Factory",
    );
  });

  it("keeps the raw identity when the bot has no display_name", () => {
    expect(botFilterLabel("adhoc-script", catalog)).toBe("adhoc-script");
  });

  it("keeps the raw identity when nothing in the catalog matches", () => {
    expect(botFilterLabel("mystery_workflow", catalog)).toBe(
      "mystery_workflow",
    );
    expect(botFilterLabel("mystery_workflow", null)).toBe("mystery_workflow");
  });
});

describe("botFilterOptions", () => {
  it("keeps raw values while humanizing labels, sorted by readable text", () => {
    const opts = botFilterOptions(
      ["whats-next", "adhoc-script", "pipeline-board-demo"],
      catalog,
    );
    expect(opts).toEqual([
      { value: "adhoc-script", label: "adhoc-script" },
      { value: "pipeline-board-demo", label: "🎬 Episode Factory" },
      { value: "whats-next", label: "🧭 Nexie" },
    ]);
  });

  it("disambiguates canon-colliding identities that share a persona label", () => {
    // A bundle launch (bot_id, kebab) and a loose CLI run (workflow-name
    // fallback, snake) of the same bot must stay separately selectable —
    // filtering is an exact match per raw identity.
    const opts = botFilterOptions(
      ["pipeline-board-demo", "pipeline_board_demo"],
      catalog,
    );
    expect(opts.map((o) => o.label).sort()).toEqual([
      "🎬 Episode Factory (pipeline-board-demo)",
      "🎬 Episode Factory (pipeline_board_demo)",
    ]);
    expect(opts.map((o) => o.value).sort()).toEqual([
      "pipeline-board-demo",
      "pipeline_board_demo",
    ]);
  });

  it("never mangles unique raw labels", () => {
    const opts = botFilterOptions(["alpha", "beta"], null);
    expect(opts).toEqual([
      { value: "alpha", label: "alpha" },
      { value: "beta", label: "beta" },
    ]);
  });
});
