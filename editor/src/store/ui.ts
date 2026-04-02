import { create } from "zustand";

export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow" | "comments";
export type LayoutDirection = "DOWN" | "RIGHT";

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
  toasts: Toast[];
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
  toggleDiagnosticsPanel: () => void;
  toggleExpanded: () => void;
  setBrowserFullscreen: (value: boolean) => void;
  setActiveWorkflowName: (name: string | null) => void;
  setLayoutDirection: (dir: LayoutDirection) => void;
  toggleLayoutDirection: () => void;
  addToast: (message: string, type: Toast["type"]) => void;
  removeToast: (id: number) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  diagnosticsPanelOpen: true,
  expanded: false,
  browserFullscreen: false,
  activeWorkflowName: null,
  layoutDirection: "DOWN",
  toasts: [],
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
  toggleDiagnosticsPanel: () => set((s) => ({ diagnosticsPanelOpen: !s.diagnosticsPanelOpen })),
  toggleExpanded: () => set((s) => ({ expanded: !s.expanded })),
  setBrowserFullscreen: (value) => set({ browserFullscreen: value }),
  setActiveWorkflowName: (activeWorkflowName) => set({ activeWorkflowName }),
  setLayoutDirection: (layoutDirection) => set({ layoutDirection }),
  toggleLayoutDirection: () => set((s) => ({ layoutDirection: s.layoutDirection === "DOWN" ? "RIGHT" : "DOWN" })),
  addToast: (message, type) => {
    const id = ++toastIdCounter;
    set((s) => ({ toasts: [...s.toasts, { id, message, type }] }));
    setTimeout(() => {
      set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
    }, 3000);
  },
  removeToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}));
