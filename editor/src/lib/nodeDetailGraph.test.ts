import { beforeEach, describe, expect, it } from "vitest";
import { makeEdgeId } from "./documentToGraph";
import { DETAIL_PREFIX_EDGE, parseDetailId } from "./nodeDetailGraph";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";

beforeEach(() => {
  useSelectionStore.setState({
    selectedNodeId: null,
    selectedEdgeId: null,
    copiedNodeId: null,
  });
  useUIStore.setState({ editingItem: null, subNodeViewStack: [] });
});

describe("detail edge node click handling", () => {
  it("parses edge detail IDs so Canvas does not fall through to selecting the synthetic node", () => {
    expect(parseDetailId(`${DETAIL_PREFIX_EDGE}workflow:0`)).toEqual({
      kind: "edge",
      workflowName: "workflow",
      edgeIndex: 0,
    });
  });

  it("preserves DetailSubNode's edge selection when the ReactFlow node click follows", () => {
    const workflowName = "workflow";
    const edgeIndex = 0;
    const syntheticNodeId = `${DETAIL_PREFIX_EDGE}${workflowName}:${edgeIndex}`;

    useUIStore.setState({ editingItem: { kind: "prompt", name: "previous_prompt" }, subNodeViewStack: ["streak_check"] });

    // DetailSubNode.handleClick runs first: select the real document edge and exit detail view.
    useSelectionStore.getState().setSelectedEdge(makeEdgeId(workflowName, edgeIndex));
    useUIStore.getState().clearSubNodeView();

    // Then ReactFlow invokes Canvas.onNodeClick. Recognized detail IDs return early and must not
    // overwrite the real edge selection with the synthetic detail node id.
    const detail = parseDetailId(syntheticNodeId);
    if (!detail) useSelectionStore.getState().setSelectedNode(syntheticNodeId);

    expect(useSelectionStore.getState().selectedEdgeId).toBe(makeEdgeId(workflowName, edgeIndex));
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();
    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useUIStore.getState().subNodeViewStack).toEqual([]);
  });
});
