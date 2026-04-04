import { create } from "zustand";
import type { LayerKind } from "@/lib/constants";
import { TOAST_DURATION_DEFAULT_MS } from "@/lib/constants";

export type { LayerKind };
export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow" | "comments";
export type LayoutDirection = "DOWN" | "RIGHT";
export interface EditingItem { kind: "schema" | "prompt" | "var"; name: string }

export interface Toast {
  id: number;
  message: string;
  type: "success" | "error" | "info";
}

let toastIdCounter = 0;

interface UIState {
  activeTab: SidebarTab;
  sourceViewOpen: boolean;
  diagnosticsPanelOpen: boolean;
  expanded: boolean;
  browserFullscreen: boolean;
  activeWorkflowName: string | null;
  layoutDirection: LayoutDirection;
  activeLayers: Set<LayerKind>;
  detailNodeId: string | null;
  editingItem: EditingItem | null;
  toasts: Toast[];
  // Sub-node detail view (double-click navigation)
  subNodeViewStack: string[];
  // Library panel
  libraryExpanded: boolean;
  // Edge editing in modal
  editModalEdgeInfo: { workflowName: string; edgeIndex: number } | null;
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
  toggleDiagnosticsPanel: () => void;
  toggleExpanded: () => void;
  setBrowserFullscreen: (value: boolean) => void;
  setActiveWorkflowName: (name: string | null) => void;
  setLayoutDirection: (dir: LayoutDirection) => void;
  toggleLayoutDirection: () => void;
  toggleLayer: (layer: LayerKind) => void;
  setDetailNodeId: (id: string | null) => void;
  setEditingItem: (item: EditingItem | null) => void;
  addToast: (message: string, type: Toast["type"]) => void;
  removeToast: (id: number) => void;
  // Sub-node view navigation
  pushSubNodeView: (nodeId: string) => void;
  popSubNodeView: () => void;
  clearSubNodeView: () => void;
  navigateSubNodeViewTo: (index: number) => void;
  // Library panel
  toggleLibraryPanel: () => void;
  // Edge modal
  setEditModalEdgeInfo: (info: { workflowName: string; edgeIndex: number } | null) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  diagnosticsPanelOpen: true,
  expanded: false,
  browserFullscreen: false,
  activeWorkflowName: null,
  layoutDirection: "DOWN",
  activeLayers: new Set<LayerKind>(),
  detailNodeId: null,
  editingItem: null,
  toasts: [],
  subNodeViewStack: [],
  libraryExpanded: false,
  editModalEdgeInfo: null,
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
  toggleDiagnosticsPanel: () => set((s) => ({ diagnosticsPanelOpen: !s.diagnosticsPanelOpen })),
  toggleExpanded: () => set((s) => ({ expanded: !s.expanded })),
  setBrowserFullscreen: (value) => set({ browserFullscreen: value }),
  setActiveWorkflowName: (activeWorkflowName) => set({ activeWorkflowName }),
  setLayoutDirection: (layoutDirection) => set({ layoutDirection }),
  toggleLayoutDirection: () => set((s) => ({ layoutDirection: s.layoutDirection === "DOWN" ? "RIGHT" : "DOWN" })),
  toggleLayer: (layer) => set((s) => {
    const next = new Set(s.activeLayers);
    if (next.has(layer)) next.delete(layer); else next.add(layer);
    return { activeLayers: next };
  }),
  setDetailNodeId: (detailNodeId) => set({ detailNodeId }),
  setEditingItem: (editingItem) => set({ editingItem }),
  addToast: (message, type) => {
    const id = ++toastIdCounter;
    set((s) => ({ toasts: [...s.toasts, { id, message, type }] }));
    setTimeout(() => {
      set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
    }, TOAST_DURATION_DEFAULT_MS);
  },
  removeToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
  // Sub-node view navigation
  pushSubNodeView: (nodeId) => set((s) => {
    // Prevent duplicate: ignore if already at top of stack
    if (s.subNodeViewStack.length > 0 && s.subNodeViewStack[s.subNodeViewStack.length - 1] === nodeId) {
      return s;
    }
    return { subNodeViewStack: [...s.subNodeViewStack, nodeId], detailNodeId: null, editModalEdgeInfo: null };
  }),
  popSubNodeView: () => set((s) => ({
    subNodeViewStack: s.subNodeViewStack.slice(0, -1),
    detailNodeId: null,
    editModalEdgeInfo: null,
  })),
  clearSubNodeView: () => set({ subNodeViewStack: [], detailNodeId: null, editModalEdgeInfo: null }),
  navigateSubNodeViewTo: (index) => set((s) => ({
    subNodeViewStack: s.subNodeViewStack.slice(0, index + 1),
    detailNodeId: null,
    editModalEdgeInfo: null,
  })),
  // Library panel
  toggleLibraryPanel: () => set((s) => ({ libraryExpanded: !s.libraryExpanded })),
  // Edge modal
  setEditModalEdgeInfo: (editModalEdgeInfo) => set({ editModalEdgeInfo }),
}));
