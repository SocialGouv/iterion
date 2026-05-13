import { create } from "zustand";

import { getServerInfo } from "@/api/runs";
import type { ServerInfo } from "@/api/types";

interface ServerInfoState {
  info: ServerInfo | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
}

export const useServerInfoStore = create<ServerInfoState>((set) => ({
  info: null,
  loading: false,
  error: null,
  refresh: async () => {
    set({ loading: true, error: null });
    try {
      const info = await getServerInfo();
      set({ info, loading: false });
    } catch (e) {
      set({
        loading: false,
        error: e instanceof Error ? e.message : String(e),
      });
    }
  },
}));

export function initializeServerInfo() {
  void useServerInfoStore.getState().refresh();
}
