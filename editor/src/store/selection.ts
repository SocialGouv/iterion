import { create } from "zustand";
import { useUIStore } from "./ui";

interface SelectionState {
  selectedNodeId: string | null;
  selectedEdgeId: string | null;
  copiedNodeId: string | null;
  setSelectedNode: (id: string | null) => void;
  setSelectedEdge: (id: string | null) => void;
  clearSelection: () => void;
  setCopiedNode: (id: string | null) => void;
}

// Cross-store call is safe as long as ui.ts has no reverse import —
// preserve that property if you edit either store.
function clearEditingItem() {
  useUIStore.getState().setEditingItem(null);
}

export const useSelectionStore = create<SelectionState>((set) => ({
  selectedNodeId: null,
  selectedEdgeId: null,
  copiedNodeId: null,
  setSelectedNode: (id) => {
    clearEditingItem();
    set({ selectedNodeId: id, selectedEdgeId: null });
  },
  setSelectedEdge: (id) => {
    clearEditingItem();
    set({ selectedEdgeId: id, selectedNodeId: null });
  },
  clearSelection: () => {
    clearEditingItem();
    set({ selectedNodeId: null, selectedEdgeId: null });
  },
  setCopiedNode: (id) => set({ copiedNodeId: id }),
}));
