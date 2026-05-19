import { createContext, useContext, type ReactNode, createElement } from "react";
import { create, useStore } from "zustand";

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

// createSelectionStore builds a fresh selection store. Each editor
// tab gets its own so selecting a node in one tab doesn't carry over
// to a sibling. The module-level `selectionStore` façade preserves
// the legacy singleton entry point for callers without a Provider
// in scope.
export function createSelectionStore() {
  return create<SelectionState>((set) => ({
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
}

export type SelectionStore = ReturnType<typeof createSelectionStore>;

const defaultSelectionStore = createSelectionStore();

const SelectionStoreContext = createContext<SelectionStore | null>(null);

interface SelectionStoreProviderProps {
  store: SelectionStore;
  children: ReactNode;
}

export function SelectionStoreProvider({ store, children }: SelectionStoreProviderProps) {
  return createElement(SelectionStoreContext.Provider, { value: store }, children);
}

export function useSelectionStoreInstance(): SelectionStore {
  return useContext(SelectionStoreContext) ?? defaultSelectionStore;
}

export function useSelectionStore<T>(selector: (state: SelectionState) => T): T {
  return useStore(useSelectionStoreInstance(), selector);
}

export const selectionStore = {
  getState: () => defaultSelectionStore.getState(),
  setState: defaultSelectionStore.setState,
  subscribe: defaultSelectionStore.subscribe,
};

const REGISTRY = new Map<string, SelectionStore>();

export function getOrCreateSelectionStore(tabId: string): SelectionStore {
  let store = REGISTRY.get(tabId);
  if (!store) {
    store = createSelectionStore();
    REGISTRY.set(tabId, store);
  }
  return store;
}

export function disposeSelectionStore(tabId: string): void {
  REGISTRY.delete(tabId);
}
