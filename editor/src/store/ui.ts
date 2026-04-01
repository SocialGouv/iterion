import { create } from "zustand";

export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow";

interface UIState {
  activeTab: SidebarTab;
  sourceViewOpen: boolean;
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
}));
