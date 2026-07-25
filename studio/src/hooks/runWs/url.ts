// Run-WS URL derivation, including the cross-store `?store=` override.

import { isSafeStoreParam } from "@/api/runs";
import { buildWsUrl } from "@/lib/wsUrl";

// readStoreOverrideFromURL returns the current page's `?store=` query
// param, if any. Same helper as in api/runs.ts but duplicated to avoid
// an import cycle (hooks → api → hooks would not actually cycle, but
// keeping it local minimises coupling). The WS URL must carry the
// override so the daemon's WS handler routes via resolveCrossStore
// AND streams events from the foreign store (pkg/server/runs_ws.go's
// streamEventsCrossStore path).
export function readStoreOverrideFromURL(): string {
  if (typeof window === "undefined") return "";
  try {
    const v = new URLSearchParams(window.location.search).get("store");
    // Defence-in-depth: validate the shape before forwarding to the
    // daemon WS. Server still does its own check via resolveCrossStore,
    // but a malformed value should be dropped client-side too.
    return isSafeStoreParam(v) ? (v as string) : "";
  } catch {
    return "";
  }
}

// Pure: append the store override to a derived WS URL, picking the
// right query separator.
export function appendStoreParam(wsURL: string, override: string): string {
  if (!override) return wsURL;
  const sep = wsURL.includes("?") ? "&" : "?";
  return `${wsURL}${sep}store=${encodeURIComponent(override)}`;
}

export async function deriveWsUrl(runId: string): Promise<string> {
  return appendStoreParam(
    await buildWsUrl(`/ws/runs/${encodeURIComponent(runId)}`),
    readStoreOverrideFromURL(),
  );
}
