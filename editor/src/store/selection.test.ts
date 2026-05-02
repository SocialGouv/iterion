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
