import { create } from "zustand";

interface GroupState {
  /** Set of collapsed group names. */
  collapsedGroups: Set<string>;

  toggleCollapse: (groupName: string) => void;
  collapseGroup: (groupName: string) => void;
  expandGroup: (groupName: string) => void;
  expandAll: () => void;
  collapseAll: (groupNames: string[]) => void;
}

export const useGroupStore = create<GroupState>((set) => ({
  collapsedGroups: new Set(),

  toggleCollapse: (groupName) =>
    set((s) => {
      const next = new Set(s.collapsedGroups);
      if (next.has(groupName)) next.delete(groupName);
      else next.add(groupName);
      return { collapsedGroups: next };
    }),
  collapseGroup: (groupName) =>
    set((s) => {
      const next = new Set(s.collapsedGroups);
      next.add(groupName);
      return { collapsedGroups: next };
    }),
  expandGroup: (groupName) =>
    set((s) => {
      const next = new Set(s.collapsedGroups);
      next.delete(groupName);
      return { collapsedGroups: next };
    }),
  expandAll: () => set({ collapsedGroups: new Set() }),
  collapseAll: (groupNames) => set({ collapsedGroups: new Set(groupNames) }),
}));
