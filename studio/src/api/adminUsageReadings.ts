// Usage-window readings escape hatch — super-admin. Mirrors
// pkg/server/admin_usage_readings_routes.go: forget one credential's stored
// usage readings by fingerprint, so a provider window the ledger cannot see
// was reset early stops refusing every run of that credential pre-flight.
// Clears ONE credential and leaves the global caps alone. Registered only when
// the usage-cap store is wired → a call on a server without it 404s.
//
// The DELETE answers with how many readings were dropped (0 is a valid, non-
// error outcome: the ledger had learned nothing yet). Shape from schema.ts.

import { FeatureUnavailableError, guard404, request } from "./client";
import type { components } from "./schema";

export { FeatureUnavailableError };

export type UsageReadingsClearedView =
  components["schemas"]["usageReadingsClearedView"];

export function clearUsageReadings(
  fingerprint: string,
): Promise<UsageReadingsClearedView> {
  return guard404("admin-usage-readings", () =>
    request<UsageReadingsClearedView>(
      `/admin/usage-readings/${encodeURIComponent(fingerprint)}`,
      { method: "DELETE" },
    ),
  );
}
