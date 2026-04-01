import { create } from "zustand";

export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow" | "comments";

export interface Toast {
  id: number;
  message: string;
  type: "success" | "error" | "info";
}

let toastIdCounter = 0;

interface UIState {
  activeTab: SidebarTab;
  sourceViewOpen: boolean;
  activeWorkflowName: string | null;
  toasts: Toast[];
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
  setActiveWorkflowName: (name: string | null) => void;
  addToast: (message: string, type: Toast["type"]) => void;
  removeToast: (id: number) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  activeWorkflowName: null,
  toasts: [],
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
  setActiveWorkflowName: (activeWorkflowName) => set({ activeWorkflowName }),
  addToast: (message, type) => {
    const id = ++toastIdCounter;
    set((s) => ({ toasts: [...s.toasts, { id, message, type }] }));
    setTimeout(() => {
      set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
    }, 3000);
  },
  removeToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}));
