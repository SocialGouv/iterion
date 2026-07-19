// Extracted from LaunchView.tsx to keep that file focused.
// One-shot fetch of the server info the launch form needs (upload
// limits, cloud/local mode for the repo-target section).

import { useEffect, useState } from "react";

import { getServerInfo } from "@/api/runs";
import type { ServerInfo } from "@/api/types";

export function useLaunchServerInfo(): ServerInfo | null {
  const [serverInfo, setServerInfo] = useState<ServerInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    getServerInfo()
      .then((info) => {
        if (!cancelled) setServerInfo(info);
      })
      .catch(() => {
        // Non-fatal: limits remain unknown; UI shows no bandeau and the
        // server still rejects oversized uploads on the wire.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return serverInfo;
}
