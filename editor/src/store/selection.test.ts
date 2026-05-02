import { beforeEach, describe, expect, it } from "vitest";
import { useSelectionStore } from "./selection";
import { useUIStore } from "./ui";

beforeEach(() => {
  useSelectionStore.setState({
    selectedNodeId: null,
    selectedEdgeId: null,
    copiedNodeId: null,
  });
  useUIStore.setState({ editingItem: null });
});

describe("selection store ↔ editingItem coupling", () => {
  it("setSelectedNode clears editingItem", () => {
    useUIStore.getState().setEditingItem({ kind: "prompt", name: "plan_system" });
    expect(useUIStore.getState().editingItem).not.toBeNull();

    useSelectionStore.getState().setSelectedNode("act");

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useSelectionStore.getState().selectedNodeId).toBe("act");
    expect(useSelectionStore.getState().selectedEdgeId).toBeNull();
  });

  it("setSelectedEdge clears editingItem", () => {
    useUIStore.getState().setEditingItem({ kind: "schema", name: "input" });

    useSelectionStore.getState().setSelectedEdge("vibe:0");

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useSelectionStore.getState().selectedEdgeId).toBe("vibe:0");
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();
  });

  it("clearSelection clears editingItem", () => {
    useUIStore.getState().setEditingItem({ kind: "var", name: "workspace_dir" });
    useSelectionStore.setState({ selectedNodeId: "plan", selectedEdgeId: null });

    useSelectionStore.getState().clearSelection();

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();
    expect(useSelectionStore.getState().selectedEdgeId).toBeNull();
  });

  it("setCopiedNode does NOT clear editingItem (copy is not a focus change)", () => {
    const item = { kind: "prompt" as const, name: "plan_system" };
    useUIStore.getState().setEditingItem(item);

    useSelectionStore.getState().setCopiedNode("plan");

    expect(useUIStore.getState().editingItem).toEqual(item);
    expect(useSelectionStore.getState().copiedNodeId).toBe("plan");
  });

  it("setSelectedNode and setSelectedEdge remain mutually exclusive", () => {
    useSelectionStore.getState().setSelectedEdge("e1");
    expect(useSelectionStore.getState().selectedEdgeId).toBe("e1");
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();

    useSelectionStore.getState().setSelectedNode("foo");
    expect(useSelectionStore.getState().selectedNodeId).toBe("foo");
    expect(useSelectionStore.getState().selectedEdgeId).toBeNull();

    useSelectionStore.getState().setSelectedEdge("e2");
    expect(useSelectionStore.getState().selectedEdgeId).toBe("e2");
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();
  });

  it("setSelectedNode(null) still clears editingItem (regression for over-narrow guards)", () => {
    useUIStore.getState().setEditingItem({ kind: "prompt", name: "x" });
    useSelectionStore.getState().setSelectedNode(null);
    expect(useUIStore.getState().editingItem).toBeNull();
  });
});

describe("subNodeView navigation ↔ editingItem coupling", () => {
  // The Inspector's mode hook prioritizes editingItem unconditionally, so
  // any sub-node-view "exit / change scope" action must drop editingItem.
  // Otherwise the right panel stays pinned to the previously-edited item
  // (e.g. a prompt clicked inside streak_check's subview) until the user
  // manually clicks "Back" — even after picking a different node.

  it("clearSubNodeView clears editingItem", () => {
    useUIStore.setState({
      editingItem: { kind: "prompt", name: "x" },
      subNodeViewStack: ["streak_check"],
    });

    useUIStore.getState().clearSubNodeView();

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useUIStore.getState().subNodeViewStack).toEqual([]);
  });

  it("popSubNodeView clears editingItem", () => {
    useUIStore.setState({
      editingItem: { kind: "schema", name: "review_input" },
      subNodeViewStack: ["a", "b"],
    });

    useUIStore.getState().popSubNodeView();

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useUIStore.getState().subNodeViewStack).toEqual(["a"]);
  });

  it("navigateSubNodeViewTo clears editingItem", () => {
    useUIStore.setState({
      editingItem: { kind: "var", name: "workspace_dir" },
      subNodeViewStack: ["a", "b", "c"],
    });

    useUIStore.getState().navigateSubNodeViewTo(0);

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useUIStore.getState().subNodeViewStack).toEqual(["a"]);
  });

  it("pushSubNodeView does NOT clear editingItem (entering a deeper view shouldn't drop edit focus)", () => {
    const item = { kind: "prompt" as const, name: "plan_system" };
    useUIStore.setState({ editingItem: item, subNodeViewStack: [] });

    useUIStore.getState().pushSubNodeView("plan");

    expect(useUIStore.getState().editingItem).toEqual(item);
    expect(useUIStore.getState().subNodeViewStack).toEqual(["plan"]);
  });
});
