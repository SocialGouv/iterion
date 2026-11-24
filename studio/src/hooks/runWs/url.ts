// Run-WS URL derivation, including the cross-store `?store=` override.

import { withExpectedBirth } from "@/lib/lostRuns";
import { appendStoreParam, readStoreOverrideFromURL, storeOverrideFor } from "@/lib/storeOverride";
import { buildWsUrl } from "@/lib/wsUrl";

// The page's override and the query-appending helper live in
// @/lib/storeOverride, one definition for the API client and the socket. The
// WS URL of the page's run carries the override so the daemon's WS handler
// routes via resolveCrossStore AND streams events from the foreign store.
export { appendStoreParam, readStoreOverrideFromURL };

// deriveWsUrl carries the page's store override (for the page's run alone)
// and, for a run of the daemon's own store, its expected birth, read at each
// dial: the server closes a socket whose run was replaced (run_origin_lost,
// then scope_lost), so a redial — on any replica — binds the run the page
// meant or none.
export async function deriveWsUrl(runId: string): Promise<string> {
  return withExpectedBirth(
    appendStoreParam(await buildWsUrl(`/ws/runs/${encodeURIComponent(runId)}`), storeOverrideFor(runId)),
  );
}
