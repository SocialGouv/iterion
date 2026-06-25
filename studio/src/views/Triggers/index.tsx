import { useCallback, useEffect, useMemo, useState } from "react";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { Badge } from "@/components/ui/Badge";
import { Checkbox } from "@/components/ui/Checkbox";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { useConfirm } from "@/hooks/useConfirm";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { errorMessage } from "@/lib/errorHints";
import {
  listTriggers,
  deleteTrigger,
  setTriggerEnabled,
  FeatureUnavailableError,
  type TriggerSubscription,
} from "@/api/triggers";
import NewTriggerDialog from "./NewTriggerDialog";

// sourceTone maps a trigger source to a Badge tone so the table reads at a
// glance (board promotes, run chains, schedule/cron, forge observational,
// custom ingress).
function sourceLabel(sub: TriggerSubscription): string {
  const s = sub.match?.sources?.[0];
  if (s) return s;
  // Derive from invocation when the matcher doesn't pin a source.
  return sub.invocation === "schedule" ? "schedule" : sub.invocation === "board" ? "board" : "—";
}

function matchSummary(sub: TriggerSubscription): string {
  const m = sub.match ?? {};
  const parts: string[] = [];
  if (sub.cron) parts.push(`cron ${sub.cron}`);
  if (m.kinds?.length) parts.push(m.kinds.join("/"));
  if (m.subject_states?.length) parts.push(`state ∈ {${m.subject_states.join(",")}}`);
  if (m.labels?.length) parts.push(`labels ⊇ {${m.labels.join(",")}}`);
  if (m.actions?.length) parts.push(`action ∈ {${m.actions.join(",")}}`);
  if (m.authors?.length) parts.push(`author ∈ {${m.authors.join(",")}}`);
  return parts.join(" · ") || "any";
}

export default function TriggersView() {
  const [subs, setSubs] = useState<TriggerSubscription[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [creating, setCreating] = useState(false);
  const { confirm, dialog } = useConfirm();
  const action = useAsyncAction();

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

  const onToggle = useCallback(
    async (sub: TriggerSubscription, enabled: boolean) => {
      await action.run(() => setTriggerEnabled(sub, enabled));
      void reload();
    },
    [action, reload],
  );

  const onDelete = useCallback(
    async (sub: TriggerSubscription) => {
      const ok = await confirm({
        title: "Delete trigger",
        message: `Stop launching ${sub.bot_id} from this ${sub.invocation} trigger?`,
        confirmLabel: "Delete",
        confirmVariant: "danger",
      });
      if (!ok) return;
      await action.run(() => deleteTrigger(sub.id));
      void reload();
    },
    [action, confirm, reload],
  );

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
      {action.error && (
        <InlineBanner tone="danger" title="Action failed">
          {action.error}
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
        <div className="overflow-x-auto rounded-md border border-border-default">
          <table className="w-full text-sm">
            <thead className="bg-surface-2 text-left text-xs text-fg-muted">
              <tr>
                <th className="px-3 py-2 font-medium">On</th>
                <th className="px-3 py-2 font-medium">Source</th>
                <th className="px-3 py-2 font-medium">Bot</th>
                <th className="px-3 py-2 font-medium">When</th>
                <th className="px-3 py-2 font-medium">Mode</th>
                <th className="px-3 py-2 font-medium">Origin</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {rows.map((sub) => (
                <tr key={sub.id} className="border-t border-border-subtle">
                  <td className="px-3 py-2">
                    <Checkbox
                      checked={sub.enabled}
                      onChange={(e) => void onToggle(sub, e.target.checked)}
                      aria-label={sub.enabled ? "Disable trigger" : "Enable trigger"}
                    />
                  </td>
                  <td className="px-3 py-2">
                    <Badge>{sourceLabel(sub)}</Badge>
                  </td>
                  <td className="px-3 py-2 font-medium text-fg-default">
                    {sub.bot_id}
                    {sub.repo ? <span className="text-fg-muted"> · {sub.repo}</span> : null}
                  </td>
                  <td className="px-3 py-2 text-fg-muted">{matchSummary(sub)}</td>
                  <td className="px-3 py-2 text-fg-muted">{sub.mode || "direct"}</td>
                  <td className="px-3 py-2 text-fg-muted">{sub.origin || "operator"}</td>
                  <td className="px-3 py-2 text-right">
                    <Button variant="ghost" size="sm" onClick={() => void onDelete(sub)}>
                      Delete
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <NewTriggerDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={() => {
          setCreating(false);
          void reload();
        }}
      />
      {dialog}
    </div>
  );
}
