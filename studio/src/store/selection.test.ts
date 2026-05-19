import { beforeEach, describe, expect, it } from "vitest";
import { selectionStore } from "./selection";
import { useUIStore } from "./ui";

beforeEach(() => {
  selectionStore.setState({
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

    selectionStore.getState().setSelectedNode("act");

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(selectionStore.getState().selectedNodeId).toBe("act");
    expect(selectionStore.getState().selectedEdgeId).toBeNull();
  });

  it("setSelectedEdge clears editingItem", () => {
    useUIStore.getState().setEditingItem({ kind: "schema", name: "input" });

    selectionStore.getState().setSelectedEdge("vibe:0");

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(selectionStore.getState().selectedEdgeId).toBe("vibe:0");
    expect(selectionStore.getState().selectedNodeId).toBeNull();
  });

  it("clearSelection clears editingItem", () => {
    useUIStore.getState().setEditingItem({ kind: "var", name: "workspace_dir" });
    selectionStore.setState({ selectedNodeId: "plan", selectedEdgeId: null });

    selectionStore.getState().clearSelection();

    expect(useUIStore.getState().editingItem).toBeNull();
    expect(selectionStore.getState().selectedNodeId).toBeNull();
    expect(selectionStore.getState().selectedEdgeId).toBeNull();
  });

  it("setCopiedNode does NOT clear editingItem (copy is not a focus change)", () => {
    const item = { kind: "prompt" as const, name: "plan_system" };
    useUIStore.getState().setEditingItem(item);

    selectionStore.getState().setCopiedNode("plan");

    expect(useUIStore.getState().editingItem).toEqual(item);
    expect(selectionStore.getState().copiedNodeId).toBe("plan");
  });

  it("setSelectedNode and setSelectedEdge remain mutually exclusive", () => {
    selectionStore.getState().setSelectedEdge("e1");
    expect(selectionStore.getState().selectedEdgeId).toBe("e1");
    expect(selectionStore.getState().selectedNodeId).toBeNull();

    selectionStore.getState().setSelectedNode("foo");
    expect(selectionStore.getState().selectedNodeId).toBe("foo");
    expect(selectionStore.getState().selectedEdgeId).toBeNull();

    selectionStore.getState().setSelectedEdge("e2");
    expect(selectionStore.getState().selectedEdgeId).toBe("e2");
    expect(selectionStore.getState().selectedNodeId).toBeNull();
  });

  it("setSelectedNode(null) still clears editingItem (regression for over-narrow guards)", () => {
    useUIStore.getState().setEditingItem({ kind: "prompt", name: "x" });
    selectionStore.getState().setSelectedNode(null);
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
