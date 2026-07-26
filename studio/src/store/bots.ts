import { errorMessage } from "@/lib/errorHints";
import { create } from "zustand";

import {
  listBots,
  setBotOverlay,
  updateBot,
  type BotEntryWithSchema,
  type BotPatch,
} from "@/api/bots";

// Single source of truth for the bot catalog across Home, the board
// BotPicker, the Inspector "Bot" tab, and the Catalog manager. Replaces
// the per-component one-shot fetches so a metadata edit or a catalog
// toggle re-renders every surface.
interface BotsState {
  bots: BotEntryWithSchema[] | null;
  loading: boolean;
  error: string | null;
  /** Fetch once (no-op if already loaded or in flight). */
  fetch: () => Promise<void>;
  /** Force a new request that supersedes any in-flight catalog load. */
  refetch: () => Promise<void>;
  /** Persist manifest metadata, then refresh the cache. Returns the
   *  updated entry; throws on failure (caller toasts). */
  saveBot: (name: string, patch: BotPatch) => Promise<BotEntryWithSchema>;
  /** Toggle workspace catalog visibility via the overlay, then refresh. */
  setOverlay: (name: string, enabled: boolean | null) => Promise<BotEntryWithSchema>;
}

let inflight: Promise<void> | null = null;
// Monotonic request stamp: only the LATEST load may write the store.
// Without it, a slow initial fetch scoped to the previous project can
// resolve after the project-switch refetch and clobber the new catalog
// with stale bots for the rest of the session (fetch() no-ops once
// bots !== null).
let loadSeq = 0;

async function load(set: (partial: Partial<BotsState>) => void): Promise<void> {
  const seq = ++loadSeq;
  set({ loading: true, error: null });
  try {
    const bots = await listBots();
    if (seq !== loadSeq) return; // superseded (e.g. project switch)
    set({ bots, loading: false });
  } catch (e) {
    if (seq !== loadSeq) return;
    set({ error: errorMessage(e), loading: false });
  }
}

export const useBotsStore = create<BotsState>((set, get) => ({
  bots: null,
  loading: false,
  error: null,
  fetch: () => {
    if (get().bots !== null) return Promise.resolve();
    if (inflight) return inflight;
    inflight = load(set).finally(() => {
      inflight = null;
    });
    return inflight;
  },
  refetch: () => load(set),
  saveBot: async (name, patch) => {
    const updated = await updateBot(name, patch);
    set((s) => ({ bots: mergeEntry(s.bots, updated) }));
    return updated;
  },
  setOverlay: async (name, enabled) => {
    const updated = await setBotOverlay(name, enabled);
    set((s) => ({ bots: mergeEntry(s.bots, updated) }));
    return updated;
  },
}));

// mergeEntry replaces the matching entry in place (or appends) so the
// cached list reflects a saved bot without a full refetch.
function mergeEntry(
  bots: BotEntryWithSchema[] | null,
  updated: BotEntryWithSchema,
): BotEntryWithSchema[] {
  if (!bots) return [updated];
  const i = bots.findIndex((b) => b.name === updated.name);
  if (i === -1) return [...bots, updated];
  const next = bots.slice();
  next[i] = updated;
  return next;
}
