// @vitest-environment jsdom
import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { NativeBoard, NativeIssue } from "@/api/native";

import { useSwimlanes } from "./useSwimlanes";

const board = {
  states: [{ name: "backlog" }, { name: "done" }],
} as unknown as NativeBoard;

function issue(id: string, bot?: string): NativeIssue {
  return { id, title: id, state: "backlog", bot } as unknown as NativeIssue;
}

describe("useSwimlanes bot grouping", () => {
  it("lands a card with a bot in its own lane and a bot-less card in the No-bot lane", () => {
    const issues = [issue("a", "feature-dev"), issue("b")];
    const { result } = renderHook(() =>
      useSwimlanes({
        board,
        filteredIssues: issues,
        groupMode: "bot",
        sortMode: "priority",
      }),
    );

    const lanes = result.current!;
    const byKey = new Map(lanes.map((l) => [l.key, l]));

    // Named-bot lane holds card "a".
    const featureLane = byKey.get("feature-dev")!;
    expect(featureLane.label).toBe("feature-dev");
    expect(featureLane.byState.get("backlog")!.map((i) => i.id)).toEqual(["a"]);

    // The bot-less card falls into the sentinel "No bot" lane.
    const noneLane = byKey.get("__none__")!;
    expect(noneLane.label).toBe("No bot");
    expect(noneLane.byState.get("backlog")!.map((i) => i.id)).toEqual(["b"]);

    // Named lanes sort before the No-bot lane (LANE_NONE always last).
    expect(lanes[lanes.length - 1]!.key).toBe("__none__");
  });
});
