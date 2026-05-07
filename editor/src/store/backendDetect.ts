import { create } from "zustand";

import { fetchBackendDetect, type BackendDetectReport } from "@/api/backends";

interface BackendDetectState {
  report: BackendDetectReport | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
}

export const useBackendDetectStore = create<BackendDetectState>((set) => ({
  report: null,
  loading: false,
  error: null,
  refresh: async () => {
    set({ loading: true, error: null });
    try {
      const report = await fetchBackendDetect();
      set({ report, loading: false });
    } catch (e) {
      set({
        loading: false,
        error: e instanceof Error ? e.message : String(e),
      });
    }
  },
}));

// initializeBackendDetect kicks off the first probe. Called once from App
// at mount time. We deliberately do NOT poll on focus — the server caches
// 30s and the user rarely changes credentials mid-session. They get a
// manual refresh by clicking the BackendStatusPill.
export function initializeBackendDetect() {
  void useBackendDetectStore.getState().refresh();
}
