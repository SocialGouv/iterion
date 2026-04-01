import { create } from "zustand";

interface SelectionState {
  selectedNodeId: string | null;
  selectedEdgeId: string | null;
  copiedNodeId: string | null;
  setSelectedNode: (id: string | null) => void;
  setSelectedEdge: (id: string | null) => void;
  clearSelection: () => void;
  setCopiedNode: (id: string | null) => void;
}

export const useSelectionStore = create<SelectionState>((set) => ({
  selectedNodeId: null,
  selectedEdgeId: null,
  copiedNodeId: null,
  setSelectedNode: (id) => set({ selectedNodeId: id, selectedEdgeId: null }),
  setSelectedEdge: (id) => set({ selectedEdgeId: id, selectedNodeId: null }),
  clearSelection: () => set({ selectedNodeId: null, selectedEdgeId: null }),
  setCopiedNode: (id) => set({ copiedNodeId: id }),
}));
