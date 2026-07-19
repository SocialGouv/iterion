// Extracted from LaunchView.tsx to keep that file focused.
// One-shot fetch of the server info the launch form needs (upload
// limits, cloud/local mode for the repo-target section).

import { useQuery } from "@tanstack/react-query";

import { getServerInfo } from "@/api/runs";
import type { ServerInfo } from "@/api/types";

export function useLaunchServerInfo(): ServerInfo | null {
  // A fetch failure is non-fatal by design: limits remain unknown (null),
  // the UI shows no bandeau, and the server still rejects oversized
  // uploads on the wire.
  const query = useQuery({
    queryKey: ["server-info"],
    queryFn: getServerInfo,
  });
  return query.data ?? null;
}
