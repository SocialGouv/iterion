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
  searchQuery: string;
  activeCategory: LibraryCategory | null;

  setSearchQuery: (q: string) => void;
  setActiveCategory: (cat: LibraryCategory | null) => void;
  addCustomItem: (item: LibraryItem) => void;
  removeCustomItem: (id: string) => void;
  updateCustomItem: (id: string, updates: Partial<LibraryItem>) => void;
}

/** Selector: all items (presets + custom). Stable when inputs don't change. */
export function selectAllItems(s: LibraryState): LibraryItem[] {
  return [...s.presetItems, ...s.customItems];
}

/** Selector: filtered items based on active category and search query. */
export function selectFilteredItems(s: LibraryState): LibraryItem[] {
  let items = selectAllItems(s);
  if (s.activeCategory) {
    items = items.filter((i) => i.category === s.activeCategory);
  }
  if (s.searchQuery.trim()) {
    const q = s.searchQuery.toLowerCase();
    items = items.filter(
      (i) =>
        i.name.toLowerCase().includes(q) ||
        i.description.toLowerCase().includes(q) ||
        i.tags?.some((t) => t.toLowerCase().includes(q)),
    );
  }
  return items;
}

/** Selector factory: find an item by id. */
export function selectItemById(id: string) {
  return (s: LibraryState): LibraryItem | undefined =>
    selectAllItems(s).find((i) => i.id === id);
}

export const useLibraryStore = create<LibraryState>((set) => ({
  presetItems: PRESET_ITEMS,
  customItems: loadCustomItems(),
  searchQuery: "",
  activeCategory: null,

  setSearchQuery: (searchQuery) => set({ searchQuery }),
  setActiveCategory: (activeCategory) => set({ activeCategory }),

  addCustomItem: (item) =>
    set((s) => {
      const customItems = [...s.customItems, item];
      saveCustomItems(customItems);
      return { customItems };
    }),

  removeCustomItem: (id) =>
    set((s) => {
      const customItems = s.customItems.filter((i) => i.id !== id);
      saveCustomItems(customItems);
      return { customItems };
    }),

  updateCustomItem: (id, updates) =>
    set((s) => {
      const customItems = s.customItems.map((i) => (i.id === id ? { ...i, ...updates } : i));
      saveCustomItems(customItems);
      return { customItems };
    }),
}));
