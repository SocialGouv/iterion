import { useCallback, useEffect, useMemo, useState } from "react";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { errorMessage } from "@/lib/errorHints";
import {
  listTriggers,
  FeatureUnavailableError,
  type TriggerSubscription,
} from "@/api/triggers";
import NewTriggerDialog from "./NewTriggerDialog";
import TriggerList from "./TriggerList";

export default function TriggersView() {
  const [subs, setSubs] = useState<TriggerSubscription[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [creating, setCreating] = useState(false);

  const reload = useCallback(async () => {
    try {
      const list = await listTriggers();
      setSubs(list);
      setLoadErr(null);
    } catch (err) {
      if (err instanceof FeatureUnavailableError) {
        setUnavailable(true);
        return;
      }
      setLoadErr(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Automations</span>,
    right: (
      <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
        New trigger
      </Button>
    ),
  });

  const rows = useMemo(() => subs ?? [], [subs]);

  if (unavailable) {
    return (
      <div className="p-6">
        <EmptyState
          title="Automations not enabled"
          message="This server has no trigger store wired. Launch a studio with the native tracker to manage event-driven triggers."
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3 p-4">
      <p className="text-xs text-fg-muted">
        Event-driven triggers fire a bot when something happens — a board card moves, a run finishes,
        a cron tick, a forge event, or a custom integration. Managed by repo and by bot on one spine.
      </p>

      {loadErr && (
        <InlineBanner tone="danger" title="Couldn't load triggers">
          {loadErr}
        </InlineBanner>
      )}

      {subs === null && !loadErr ? (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading triggers…
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          title="No triggers yet"
          message="Create one to launch a bot automatically — e.g. when a card enters “ready” with the “feature” label, or after another bot finishes."
          action={
            <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
              New trigger
            </Button>
          }
        />
      ) : (
        <TriggerList subs={rows} onChanged={() => void reload()} />
      )}

      <NewTriggerDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={() => {
          setCreating(false);
          void reload();
        }}
      />
    </div>
  );
}
