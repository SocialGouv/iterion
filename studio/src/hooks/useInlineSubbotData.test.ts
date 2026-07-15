import { describe, expect, it } from "vitest";

import type { RunSummary, WireWorkflow } from "@/api/runs";
import { deriveNextFrames } from "./useInlineSubbotData";

// Pure-logic tests for the nested-level derivation; the hook plumbing
// itself (react-query fan-out) stays manual/Playwright like the rest of
// the canvas stack.

function child(partial: Partial<RunSummary>): RunSummary {
  return {
    id: "run-1",
    workflow_name: "step",
    status: "running",
    created_at: "2026-07-15T10:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    active: true,
    parent_run_id: "parent-1",
    ...partial,
  };
}

const STAGE_WF: WireWorkflow = {
  name: "stage",
  entry: "split",
  nodes: [
    { id: "split", kind: "tool" },
    { id: "step", kind: "subbot", source: "step.bot" },
    { id: "done", kind: "done" },
  ],
  edges: [
    { from: "split", to: "step" },
    { from: "step", to: "done" },
  ],
};

describe("deriveNextFrames", () => {
  it("derives nested frames from the selected child's children, keyed by chained id", () => {
    const frames = [
      {
        frameId: "stage",
        children: [child({ id: "stage-run" })],
      },
    ];
    const next = deriveNextFrames(frames, {
      wfByFrame: new Map([["stage", STAGE_WF]]),
      childrenOfSelected: new Map([
        [
          "stage",
          [
            child({ id: "g1", parent_node_id: "step" }),
            child({ id: "g2", parent_node_id: "step" }),
          ],
        ],
      ]),
    });
    expect(next).toEqual([
      {
        frameId: "stage::step",
        children: [
          expect.objectContaining({ id: "g1" }),
          expect.objectContaining({ id: "g2" }),
        ],
      },
    ]);
  });

  it("falls back to the single subbot node for legacy grandchildren without parent_node_id", () => {
    const next = deriveNextFrames(
      [{ frameId: "stage", children: [child({ id: "stage-run" })] }],
      {
        wfByFrame: new Map([["stage", STAGE_WF]]),
        childrenOfSelected: new Map([["stage", [child({ id: "legacy" })]]]),
      },
    );
    expect(next.map((f) => f.frameId)).toEqual(["stage::step"]);
  });

  it("derives nothing without a workflow or without grandchildren", () => {
    const frames = [{ frameId: "stage", children: [child({ id: "r" })] }];
    expect(
      deriveNextFrames(frames, {
        wfByFrame: new Map(),
        childrenOfSelected: new Map([["stage", [child({ id: "g1" })]]]),
      }),
    ).toEqual([]);
    expect(
      deriveNextFrames(frames, {
        wfByFrame: new Map([["stage", STAGE_WF]]),
        childrenOfSelected: new Map(),
      }),
    ).toEqual([]);
  });
});
