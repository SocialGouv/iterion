import { useCallback, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Badge } from "@/components/ui/Badge";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { useConfirm } from "@/hooks/useConfirm";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { humanizeCron } from "@/lib/humanizeCron";
import {
  deleteTrigger,
  setTriggerEnabled,
  type TriggerSubscription,
} from "@/api/triggers";
import NewTriggerDialog from "./NewTriggerDialog";

// sourceLabel maps a subscription to its at-a-glance source badge (board
// promotes, run chains, schedule/cron, forge observational, custom ingress).
function sourceLabel(sub: TriggerSubscription): string {
  const s = sub.match?.sources?.[0];
  if (s) return s;
  // Derive from invocation when the matcher doesn't pin a source.
  return sub.invocation === "schedule" ? "schedule" : sub.invocation === "board" ? "board" : "—";
}

function matchSummary(sub: TriggerSubscription): string {
  const m = sub.match ?? {};
  const parts: string[] = [];
  if (sub.cron) {
    const human = humanizeCron(sub.cron);
    parts.push(`cron ${sub.cron}${human ? ` (${human})` : ""}`);
  }
  if (m.kinds?.length) parts.push(m.kinds.join("/"));
  if (m.subject_states?.length) parts.push(`state ∈ {${m.subject_states.join(",")}}`);
  if (m.labels?.length) parts.push(`labels ⊇ {${m.labels.join(",")}}`);
  if (m.actions?.length) parts.push(`action ∈ {${m.actions.join(",")}}`);
  if (m.authors?.length) parts.push(`author ∈ {${m.authors.join(",")}}`);
  return parts.join(" · ") || "any";
}

interface Props {
  subs: TriggerSubscription[];
  /** Called after a successful mutation (toggle / delete) so the owner
   *  refetches its list. */
  onChanged: () => void;
  /** Hide the Bot column when the list is already scoped to one bot
   *  (the bot home's "Active triggers" card). */
  hideBotColumn?: boolean;
}

/**
 * TriggerList renders trigger subscriptions as the shared table used by
 * BOTH the /triggers Automations page and the bot home's Automations
 * card: enabled checkbox, source badge, match summary, edit, and delete —
 * extracted verbatim from the /triggers view so behaviour stays
 * identical on both surfaces.
 */
export default function TriggerList({ subs, onChanged, hideBotColumn = false }: Props) {
  const { confirm, dialog } = useConfirm();
  const action = useAsyncAction();
  const [editing, setEditing] = useState<TriggerSubscription | null>(null);

  const onToggle = useCallback(
    async (sub: TriggerSubscription, enabled: boolean) => {
      await action.run(() => setTriggerEnabled(sub, enabled));
      onChanged();
    },
    [action, onChanged],
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
      onChanged();
    },
    [action, confirm, onChanged],
  );

  return (
    <>
      {action.error && (
        <InlineBanner tone="danger" title="Action failed">
          {action.error}
        </InlineBanner>
      )}
      <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] overflow-hidden">
        <Table caption="Event-driven trigger subscriptions">
          <THead className="bg-surface-2">
            <Th>On</Th>
            <Th>Source</Th>
            {!hideBotColumn && <Th>Bot</Th>}
            <Th>When</Th>
            <Th>Mode</Th>
            <Th>Origin</Th>
            <Th align="right" srLabel="Actions" />
          </THead>
          <TBody>
            {subs.map((sub) => (
              <Tr key={sub.id}>
                <Td>
                  <Checkbox
                    checked={sub.enabled}
                    onChange={(e) => void onToggle(sub, e.target.checked)}
                    aria-label={sub.enabled ? "Disable trigger" : "Enable trigger"}
                  />
                </Td>
                <Td>
                  <Badge>{sourceLabel(sub)}</Badge>
                </Td>
                {!hideBotColumn && (
                  <Td className="font-medium text-fg-default">
                    {sub.bot_id}
                    {sub.repo ? <span className="text-fg-muted"> · {sub.repo}</span> : null}
                  </Td>
                )}
                <Td className="text-fg-muted">{matchSummary(sub)}</Td>
                <Td className="text-fg-muted">{sub.mode || "direct"}</Td>
                <Td className="text-fg-muted">{sub.origin || "operator"}</Td>
                <Td align="right">
                  <Button variant="ghost" size="sm" onClick={() => setEditing(sub)}>
                    Edit
                  </Button>
                  <Button variant="ghost" size="sm" onClick={() => void onDelete(sub)}>
                    Delete
                  </Button>
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      </div>
      <NewTriggerDialog
        open={editing !== null}
        onOpenChange={(o) => {
          if (!o) setEditing(null);
        }}
        editing={editing}
        onCreated={() => {
          setEditing(null);
          onChanged();
        }}
      />
      {dialog}
    </>
  );
}
