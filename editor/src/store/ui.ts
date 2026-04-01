import { create } from "zustand";

export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow";

interface UIState {
  activeTab: SidebarTab;
  sourceViewOpen: boolean;
  activeWorkflowName: string | null;
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
  setActiveWorkflowName: (name: string | null) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  activeWorkflowName: null,
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
  setActiveWorkflowName: (activeWorkflowName) => set({ activeWorkflowName }),
}));
