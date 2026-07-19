// Extracted from BotHome/index.tsx to keep that file focused.
// Automations card — suggested manifest invocations (enable-with-cron
// for schedule kinds, one-click for board kinds, informational for
// command/forge kinds) plus the bot's active trigger subscriptions.

import { useCallback, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "wouter";

import type { BotEntryWithSchema, Invocation } from "@/api/bots";
import { ApiError, FeatureUnavailableError } from "@/api/client";
import {
  createTriggerFromInvocation,
  listTriggers,
  type TriggerSubscription,
} from "@/api/triggers";
import { Badge, Button, Card, InlineBanner, Input, Spinner } from "@/components/ui";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { errorMessage } from "@/lib/errorHints";
import { humanizeCron } from "@/lib/humanizeCron";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import NewTriggerDialog from "@/views/Triggers/NewTriggerDialog";
import TriggerList from "@/views/Triggers/TriggerList";

import SectionTitle from "./SectionTitle";

// Friendly labels for the manifest invocation kinds; unknown kinds (a newer
// server) fall back to the raw value.
const INVOCATION_KIND_LABELS: Record<Invocation["kind"], string> = {
  forge: "Forge event",
  command: "Slash command",
  schedule: "Schedule",
  board: "Board event",
};

function invocationKindLabel(kind: Invocation["kind"]): string {
  return INVOCATION_KIND_LABELS[kind] ?? kind;
}

function describeBoardInvocation(inv: Invocation): string {
  const states = inv.board?.to_states ?? [];
  const labels = inv.board?.all_labels ?? [];
  const stateTxt = states.length ? states.join(" / ") : "any state";
  const labelTxt = labels.length
    ? ` with label${labels.length === 1 ? "" : "s"} ${labels.join(", ")}`
    : "";
  return `When a card enters ${stateTxt}${labelTxt}`;
}

export default function AutomationsCard({ entry }: { entry: BotEntryWithSchema }) {
  const addToast = useUIStore((s) => s.addToast);
  const { activeRepo } = useActiveRepo();
  const [adding, setAdding] = useState(false);
  // Per-invocation editable cron (schedule kinds), keyed by index.
  const [crons, setCrons] = useState<Record<number, string>>({});
  const [busyIndex, setBusyIndex] = useState<number | null>(null);
  // Per-invocation outcome note ("already enabled", explicit 400 reason).
  const [notes, setNotes] = useState<Record<number, string>>({});

  const triggersEnabled = useServerInfoStore((s) => s.info?.triggers_enabled ?? false);
  const queryClient = useQueryClient();
  // Gated on the server advertising a trigger store, skipping the
  // round-trip (and its console 404) otherwise.
  const triggersQuery = useQuery<TriggerSubscription[]>({
    queryKey: ["triggers", entry.name],
    queryFn: () => listTriggers({ bot: entry.name }),
    enabled: triggersEnabled,
  });
  const unavailable =
    !triggersEnabled || triggersQuery.error instanceof FeatureUnavailableError;
  const subs = triggersQuery.data ?? null;
  const loadErr =
    triggersQuery.error && !unavailable ? errorMessage(triggersQuery.error) : null;
  // Trigger mutations (enable / edit / delete) invalidate the whole
  // triggers scope so this card AND the gallery's badge counts refresh.
  const reload = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["triggers"] }),
    [queryClient],
  );

  const invocations = entry.invocations ?? [];

  const onEnable = async (index: number, inv: Invocation) => {
    setBusyIndex(index);
    setNotes((n) => ({ ...n, [index]: "" }));
    try {
      const cron =
        inv.kind === "schedule"
          ? (crons[index] ?? inv.schedule?.suggested_cron ?? "").trim() || undefined
          : undefined;
      await createTriggerFromInvocation(entry.name, index, cron);
      addToast(`Trigger enabled for ${entry.display_name?.trim() || entry.name}`, "success");
      await reload();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setNotes((n) => ({ ...n, [index]: "Already enabled." }));
      } else {
        setNotes((n) => ({ ...n, [index]: errorMessage(err) }));
      }
    } finally {
      setBusyIndex(null);
    }
  };

  if (unavailable) {
    return (
      <Card>
        <SectionTitle flush>Automations</SectionTitle>
        <p className="text-xs text-fg-muted">
          Automations are not enabled on this server (no trigger store wired).
        </p>
      </Card>
    );
  }

  const suggestible = invocations.length > 0;

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle flush>Automations</SectionTitle>
        <Button variant="secondary" size="sm" onClick={() => setAdding(true)}>
          Add trigger…
        </Button>
      </div>

      {suggestible && (
        <div className="mt-2">
          <h3 className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
            Suggested triggers (from the manifest)
          </h3>
          <ul className="space-y-1.5">
            {invocations.map((inv, i) => (
              <SuggestedInvocationRow
                key={i}
                inv={inv}
                note={notes[i] ?? ""}
                busy={busyIndex === i}
                cron={crons[i] ?? inv.schedule?.suggested_cron ?? ""}
                onCronChange={(v) => setCrons((c) => ({ ...c, [i]: v }))}
                onEnable={() => void onEnable(i, inv)}
              />
            ))}
          </ul>
        </div>
      )}

      <div className="mt-3">
        <h3 className="mb-1 text-caption font-semibold uppercase tracking-wider text-fg-subtle">
          Active triggers
        </h3>
        {loadErr && (
          <InlineBanner tone="danger" title="Couldn't load triggers">
            {loadErr}
          </InlineBanner>
        )}
        {subs === null && !loadErr ? (
          <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
            <Spinner /> Loading triggers…
          </div>
        ) : (subs ?? []).length === 0 ? (
          <p className="py-1 text-xs text-fg-subtle">
            No triggers yet — enable a suggested one above, or add one manually.
          </p>
        ) : (
          <TriggerList subs={subs ?? []} onChanged={() => void reload()} hideBotColumn />
        )}
      </div>

      <NewTriggerDialog
        open={adding}
        onOpenChange={setAdding}
        defaultBotId={entry.name}
        defaultRepo={activeRepo?.repo_full_name}
        onCreated={() => {
          setAdding(false);
          void reload();
        }}
      />
    </Card>
  );
}

function SuggestedInvocationRow({
  inv,
  note,
  busy,
  cron,
  onCronChange,
  onEnable,
}: {
  inv: Invocation;
  note: string;
  busy: boolean;
  cron: string;
  onCronChange: (v: string) => void;
  onEnable: () => void;
}) {
  if (inv.kind === "schedule") {
    const human = cron.trim() ? humanizeCron(cron.trim()) : null;
    return (
      <li className="flex flex-wrap items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
        <Badge variant="info">{invocationKindLabel("schedule")}</Badge>
        <span className="min-w-0 flex-1 text-xs text-fg-default">
          {human ? `Runs ${human}` : "Runs on a cron cadence"}
        </span>
        <Input
          type="text"
          value={cron}
          onChange={(e) => onCronChange(e.target.value)}
          placeholder="0 7 * * 1-5"
          aria-label="Cron expression (5-field)"
          size="sm"
          className="w-32 font-mono"
        />
        <Button variant="secondary" size="sm" onClick={onEnable} disabled={busy} loading={busy}>
          Enable
        </Button>
        {note && <span className="w-full text-caption text-warning">{note}</span>}
      </li>
    );
  }
  if (inv.kind === "board") {
    if (!inv.board) {
      return (
        <li className="flex flex-wrap items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5 opacity-70">
          <Badge>{invocationKindLabel("board")}</Badge>
          <span className="min-w-0 flex-1 text-xs text-fg-muted">
            Dispatcher board target — no card-event filter declared, nothing to subscribe.
          </span>
        </li>
      );
    }
    return (
      <li className="flex flex-wrap items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
        <Badge variant="info">{invocationKindLabel("board")}</Badge>
        <span className="min-w-0 flex-1 text-xs text-fg-default">{describeBoardInvocation(inv)}</span>
        <Button variant="secondary" size="sm" onClick={onEnable} disabled={busy} loading={busy}>
          Enable
        </Button>
        {note && <span className="w-full text-caption text-warning">{note}</span>}
      </li>
    );
  }
  // command / forge kinds are provisioned through the forge integration
  // flow, not from here — informational only.
  const desc =
    inv.kind === "command"
      ? `/${inv.command?.name ?? "command"} command`
      : `forge ${inv.forge?.event ?? "event"}`;
  return (
    <li className="flex flex-wrap items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5 opacity-70">
      <Badge>{invocationKindLabel(inv.kind)}</Badge>
      <span className="min-w-0 flex-1 text-xs text-fg-muted">
        {desc} — wire through the forge integration.
      </span>
      <Link href="/integrations" className="shrink-0 text-caption text-accent-text hover:underline">
        Integrations →
      </Link>
    </li>
  );
}
