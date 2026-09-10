// Extracted from ConnectRepoWizard.tsx to keep that file focused.
// Step 3 — pick which repository to enable on the freshly-made
// connection; EnableRepoPanel does the heavy lifting.

import { useEffect, useMemo, useRef } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeConnection } from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { EnableRepoPanel } from "@/views/teams/tabs/integrations/EnableRepoPanel";

export interface ReposStepProps {
  teamID: string;
  loading: boolean;
  connections: ForgeConnection[];
  connectionID: string;
  repoBots: BotEntryWithSchema[];
  reloadConnections: () => Promise<void>;
  onError: (m: string) => void;
  onDone: (enabled?: { repo: string; connectionID: string; pending?: boolean }) => void;
}

export default function ReposStep({
  teamID,
  loading,
  connections,
  connectionID,
  repoBots,
  reloadConnections,
  onError,
  onDone,
}: ReposStepProps) {
  const conn = useMemo(
    () => connections.find((c) => c.id === connectionID) ?? null,
    [connections, connectionID],
  );

  // The freshly-connected id may not be in the first list snapshot yet
  // (the connect callback races with our reload). Retry once so we don't
  // dead-end on a legitimate late-arriving connection.
  const retriedRef = useRef(false);
  useEffect(() => {
    if (!loading && !conn && connectionID && !retriedRef.current) {
      retriedRef.current = true;
      void reloadConnections();
    }
  }, [loading, conn, connectionID, reloadConnections]);

  if (loading && !conn) {
    return (
      <EmptyState
        title="Fetching your new connection…"
        message="This usually takes a second."
      />
    );
  }

  if (!conn) {
    return (
      <div className="space-y-3">
        <InlineBanner tone="warning" layout="inline">
          We couldn't find the connection that was just authorized (id{" "}
          <span className="font-mono">{connectionID || "?"}</span>). Try
          reloading — it may still be propagating.
        </InlineBanner>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => {
            retriedRef.current = false;
            void reloadConnections();
          }}
        >
          Retry
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Pick a repository</h2>
        <div className="flex items-center gap-2 text-xs text-fg-muted">
          <Badge variant="success" size="sm">
            Connected
          </Badge>
          <span>
            {conn.provider} · @{conn.account_login ?? "—"}
            {conn.forge_base_url ? ` · ${conn.forge_base_url}` : ""}
          </span>
        </div>
      </header>

      <EnableRepoPanel
        teamID={teamID}
        conn={conn}
        repoBots={repoBots}
        onDone={(enabled) => onDone(enabled)}
        onCancel={() => onDone()}
        onError={onError}
      />
    </div>
  );
}
