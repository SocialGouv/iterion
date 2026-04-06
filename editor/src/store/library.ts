import { create } from "zustand";
import type { LibraryItem, LibraryCategory } from "@/lib/library/types";
import { PRESET_ITEMS } from "@/lib/library/presets";

const STORAGE_KEY = "iterion:library-custom";

function loadCustomItems(): LibraryItem[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? JSON.parse(raw) : [];
  } catch {
    return [];
  }
}

function saveCustomItems(items: LibraryItem[]) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(items));
}

interface LibraryState {
  presetItems: LibraryItem[];
  customItems: LibraryItem[];
  /** Cached concatenation of presetItems + customItems. Referentially stable
   *  between renders so that zustand selectors don't trigger infinite loops
   *  (React 18 useSyncExternalStore requires getSnapshot to be cached). */
  _allItems: LibraryItem[];
  searchQuery: string;
  activeCategory: LibraryCategory | null;

  setSearchQuery: (q: string) => void;
  setActiveCategory: (cat: LibraryCategory | null) => void;
  addCustomItem: (item: LibraryItem) => void;
  removeCustomItem: (id: string) => void;
  updateCustomItem: (id: string, updates: Partial<LibraryItem>) => void;
}

/** Selector: all items (presets + custom). Returns the cached _allItems array. */
export function selectAllItems(s: LibraryState): LibraryItem[] {
  return s._allItems;
}

const _initialCustom = loadCustomItems();

export const useLibraryStore = create<LibraryState>((set) => ({
  presetItems: PRESET_ITEMS,
  customItems: _initialCustom,
  _allItems: [...PRESET_ITEMS, ..._initialCustom],
  searchQuery: "",
  activeCategory: null,

  setSearchQuery: (searchQuery) => set({ searchQuery }),
  setActiveCategory: (activeCategory) => set({ activeCategory }),

  addCustomItem: (item) =>
    set((s) => {
      const customItems = [...s.customItems, item];
      saveCustomItems(customItems);
      return { customItems, _allItems: [...s.presetItems, ...customItems] };
    }),

  removeCustomItem: (id) =>
    set((s) => {
      const customItems = s.customItems.filter((i) => i.id !== id);
      saveCustomItems(customItems);
      return { customItems, _allItems: [...s.presetItems, ...customItems] };
    }),

  updateCustomItem: (id, updates) =>
    set((s) => {
      const customItems = s.customItems.map((i) => (i.id === id ? { ...i, ...updates } : i));
      saveCustomItems(customItems);
      return { customItems, _allItems: [...s.presetItems, ...customItems] };
    }),
}));
