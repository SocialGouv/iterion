import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { NodeProps, Node } from "@xyflow/react";
import type { SubbotFrameData } from "@/lib/subbotGraph";

// Node-env static-markup test (house pattern: xyflow is never mounted).
// Handle needs a ReactFlow context when rendered for real, so stub it out;
// Position is a plain enum consumed by handlePositions.ts.
vi.mock("@xyflow/react", () => ({
  Handle: () => null,
  Position: { Top: "top", Right: "right", Bottom: "bottom", Left: "left" },
}));

vi.mock("wouter", () => ({
  useLocation: () => ["/", vi.fn()],
}));

vi.mock("@/store/tabs", () => ({
  useTabsStore: { getState: () => ({ openTab: vi.fn() }) },
}));

import SubbotFrameNode from "./SubbotFrameNode";

function makeProps(data: Partial<SubbotFrameData>): NodeProps<Node<SubbotFrameData, "subbotFrame">> {
  return {
    id: "produce_episode",
    type: "subbotFrame",
    data: {
      label: "produce_episode",
      kind: "subbot",
      color: "var(--color-node-subbot)",
      decl: { name: "produce_episode" },
      source: "episode.bot",
      sourcePath: "examples/pipeline-board-demo/episode.bot",
      isolated: false,
      childWorkflowName: "episode",
      ...data,
    },
    selected: false,
    dragging: false,
    draggable: true,
    selectable: true,
    deletable: true,
    isConnectable: true,
    zIndex: 0,
    positionAbsoluteX: 0,
    positionAbsoluteY: 0,
  } as NodeProps<Node<SubbotFrameData, "subbotFrame">>;
}

describe("SubbotFrameNode", () => {
  it("renders the header with name, subbot marker and mono source filename", () => {
    const html = renderToStaticMarkup(<SubbotFrameNode {...makeProps({})} />);
    expect(html).toContain("produce_episode");
    expect(html).toContain("subbot");
    expect(html).toContain("episode.bot");
    expect(html).not.toContain(">isolated<");
  });

  it("shows the isolated chip when set", () => {
    const html = renderToStaticMarkup(<SubbotFrameNode {...makeProps({ isolated: true })} />);
    expect(html).toContain(">isolated<");
  });

  it("renders an open button labeled with the source file", () => {
    const html = renderToStaticMarkup(<SubbotFrameNode {...makeProps({})} />);
    expect(html).toContain("Open episode.bot in a new editor tab");
  });
});
