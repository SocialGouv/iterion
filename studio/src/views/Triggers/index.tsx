import { useCallback, useEffect, useMemo, useState } from "react";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { Badge } from "@/components/ui/Badge";
import { Checkbox } from "@/components/ui/Checkbox";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
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

const DAY_NAMES = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
];

// humanizeCron renders the common 5-field cron shapes as a short English
// hint ("0 3 * * 1" → "every Monday at 03:00"). Deliberately conservative:
// anything outside the fixed-minute/hour + every-N + single day-of-week/
// day-of-month forms returns null and the caller shows the raw expression
// alone — a missing hint beats a wrong translation.
export function humanizeCron(expr: string): string | null {
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  // Length checked above — safe to narrow the destructuring.
  const [min, hour, dom, mon, dow] = fields as [string, string, string, string, string];
  if (mon !== "*") return null;

  const num = (s: string): number | null => (/^\d+$/.test(s) ? Number(s) : null);
  const everyN = (s: string): number | null => {
    const m = /^\*\/(\d+)$/.exec(s);
    return m ? Number(m[1]) : null;
  };
  const dayName = (s: string): string | null => {
    const n = num(s);
    // Both 0 and 7 mean Sunday in the 5-field vocabulary.
    if (n !== null) return n >= 0 && n <= 7 ? (DAY_NAMES[n % 7] ?? null) : null;
    const idx = DAY_NAMES.findIndex(
      (d) => d.slice(0, 3).toLowerCase() === s.toLowerCase(),
    );
    return idx >= 0 ? (DAY_NAMES[idx] ?? null) : null;
  };
  const pad = (n: number) => String(n).padStart(2, "0");

  const m = num(min);
  const h = num(hour);

  // Minute-cadence forms: "* * * * *" / "*/N * * * *".
  if (hour === "*" && dom === "*" && dow === "*") {
    if (min === "*") return "every minute";
    const n = everyN(min);
    if (n !== null) return n === 1 ? "every minute" : `every ${n} minutes`;
    if (m !== null) return `hourly at :${pad(m)}`;
    return null;
  }
  if (m === null) return null;

  // Hour-cadence forms: "M */N * * *".
  if (dom === "*" && dow === "*") {
    const n = everyN(hour);
    if (n !== null) {
      return n === 1 ? `hourly at :${pad(m)}` : `every ${n} hours at :${pad(m)}`;
    }
  }
  if (h === null) return null;
  const at = `${pad(h)}:${pad(m)}`;

  if (dom === "*" && dow === "*") return `daily at ${at}`;
  // Weekly: single day or a comma list of days.
  if (dom === "*") {
    const days = dow.split(",").map(dayName);
    if (days.some((d) => d === null)) return null;
    return `every ${days.join(", ")} at ${at}`;
  }
  // Monthly on a fixed day.
  if (dow === "*") {
    const d = num(dom);
    if (d === null || d < 1 || d > 31) return null;
    return `monthly on day ${d} at ${at}`;
  }
  return null;
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
        <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] overflow-hidden">
          <Table caption="Event-driven trigger subscriptions">
            <THead>
              <tr className="bg-surface-2">
                <Th>On</Th>
                <Th>Source</Th>
                <Th>Bot</Th>
                <Th>When</Th>
                <Th>Mode</Th>
                <Th>Origin</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </THead>
            <TBody>
              {rows.map((sub) => (
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
                  <Td className="font-medium text-fg-default">
                    {sub.bot_id}
                    {sub.repo ? <span className="text-fg-muted"> · {sub.repo}</span> : null}
                  </Td>
                  <Td className="text-fg-muted">{matchSummary(sub)}</Td>
                  <Td className="text-fg-muted">{sub.mode || "direct"}</Td>
                  <Td className="text-fg-muted">{sub.origin || "operator"}</Td>
                  <Td align="right">
                    <Button variant="ghost" size="sm" onClick={() => void onDelete(sub)}>
                      Delete
                    </Button>
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
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
