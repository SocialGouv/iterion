import { create } from "zustand";

const STORAGE_KEY = "iterion.recents";
const MAX_RECENTS = 10;

function readRecents(): string[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((s): s is string => typeof s === "string").slice(0, MAX_RECENTS);
  } catch {
    return [];
  }
}

function writeRecents(list: string[]) {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(list));
  } catch {
    // localStorage may be unavailable (private mode, quota); silently ignore.
  }
}

interface RecentsState {
  recents: string[];
  pushRecent: (path: string) => void;
  removeRecent: (path: string) => void;
  clearRecents: () => void;
}

export const useRecentsStore = create<RecentsState>((set) => ({
  recents: readRecents(),
  pushRecent: (path) =>
    set((s) => {
      const filtered = s.recents.filter((p) => p !== path);
      const next = [path, ...filtered].slice(0, MAX_RECENTS);
      writeRecents(next);
      return { recents: next };
    }),
  removeRecent: (path) =>
    set((s) => {
      const next = s.recents.filter((p) => p !== path);
      writeRecents(next);
      return { recents: next };
    }),
  clearRecents: () => {
    writeRecents([]);
    set({ recents: [] });
  },
}));
