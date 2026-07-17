import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useLocation, useParams } from "wouter";

import type { BotEntryWithSchema, Invocation } from "@/api/bots";
import { ApiError } from "@/api/client";
import { listBindings, type BotSecretBinding } from "@/api/botBindings";
import { listRuns, type RunSummary } from "@/api/runs";
import {
  listTriggers,
  FeatureUnavailableError,
  createTriggerFromInvocation,
  type TriggerSubscription,
} from "@/api/triggers";
import { useAuth } from "@/auth/AuthContext";
import BotMetadataForm from "@/components/Panels/BotMetadataForm";
import TestRunPane from "@/components/Runs/TestRunPane";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { STATUS_VARIANT, labelForStatus } from "@/components/Runs/runStatusMeta";
import { literalToString } from "@/components/Runs/launchView/utils";
import {
  Badge,
  Button,
  Card,
  Chip,
  EmptyState,
  InlineBanner,
  Input,
  Spinner,
  Table,
  THead,
  Th,
  TBody,
  Tr,
  Td,
} from "@/components/ui";
import { EmojiPicker } from "@/components/ui/EmojiPicker";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";
import { humanizeCron } from "@/lib/humanizeCron";
import { botVisual } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import NewTriggerDialog from "@/views/Triggers/NewTriggerDialog";
import TriggerList from "@/views/Triggers/TriggerList";

import { botLaunchFile } from "../botPaths";

/**
 * BotHomeView — /bots/:name — one bot's home page: identity + activation,
 * launch/edit/test actions, manifest metadata, automations (suggested
 * manifest invocations + active trigger subscriptions), vars, presets,
 * recent runs, and (cloud) secret bindings.
 */
export default function BotHomeView() {
  const params = useParams<{ name: string }>();
  const name = decodeURIComponent(params.name ?? "");
  const bots = useBotsStore((s) => s.bots);
  const loading = useBotsStore((s) => s.loading);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);

  useEffect(() => {
    if (bots === null) void fetchBots();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const entry = useMemo(() => (bots ?? []).find((b) => b.name === name), [bots, name]);
  const label = entry?.display_name?.trim() || name;

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/bots" className="text-fg-muted hover:text-fg-default hover:underline">
          Bots
        </Link>
        <span className="text-fg-subtle">/</span>
        <span>{label}</span>
      </span>
    ),
  });

  if (botsError) {
    return (
      <div className="p-4">
        <InlineBanner tone="danger" title="Couldn't load bots">
          {botsError}
        </InlineBanner>
      </div>
    );
  }
  if (!entry) {
    if (bots === null || loading) {
      return (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading bot…
        </div>
      );
    }
    return (
      <div className="p-6">
        <EmptyState
          title="Bot not found"
          message={`No bot named “${name}” is discovered in this workspace.`}
          action={
            <Link href="/bots">
              <Button variant="secondary" size="sm">
                Back to Bots
              </Button>
            </Link>
          }
        />
      </div>
    );
  }
  return <BotHome entry={entry} />;
}

function BotHome({ entry }: { entry: BotEntryWithSchema }) {
  const serverInfo = useServerInfoStore((s) => s.info);
  const launchFile = botLaunchFile(entry);
  const [testOpen, setTestOpen] = useState(false);

  const main = (
    <>
      <IdentityHeader entry={entry} />
      <ActionsRow
        entry={entry}
        launchFile={launchFile}
        testOpen={testOpen}
        onToggleTest={() => setTestOpen((v) => !v)}
      />
      {entry.schema_error && (
        <InlineBanner tone="warning" title="Workflow failed to parse">
          {entry.schema_error}
        </InlineBanner>
      )}
      <MetadataCard entry={entry} />
      <AutomationsCard entry={entry} />
      <VarsCard entry={entry} />
      {entry.presets && (entry.presets.entries?.length ?? 0) > 0 && (
        <PresetsCard entry={entry} />
      )}
      <RecentRunsCard botName={entry.name} />
      {serverInfo?.mode === "cloud" && <SecretBindingsCard botName={entry.name} />}
    </>
  );

  if (!testOpen || !launchFile) {
    return <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 p-4">{main}</div>;
  }

  // Test pane open: on xl the pane docks as a sticky right column next
  // to the existing sections; below xl it stacks as a bottom section.
  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4 p-4 xl:flex-row xl:items-start">
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 xl:mx-0 xl:min-w-0 xl:flex-1">
        {main}
      </div>
      <div className="w-full xl:sticky xl:top-4 xl:w-[460px] xl:shrink-0">
        <TestRunPane
          file={launchFile}
          vars={entry.vars?.fields ?? []}
          onClose={() => setTestOpen(false)}
        />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Identity header — emoji (editable), names, chips, activation toggle
// ---------------------------------------------------------------------------

function IdentityHeader({ entry }: { entry: BotEntryWithSchema }) {
  const saveBot = useBotsStore((s) => s.saveBot);
  const setOverlay = useBotsStore((s) => s.setOverlay);
  const addToast = useUIStore((s) => s.addToast);
  const [busy, setBusy] = useState(false);

  const identity = botVisual(entry);
  const label = entry.display_name?.trim() || entry.name;
  const enabled = entry.enabled !== false;
  const manifestEnabled = entry.manifest_enabled !== false;
  const overridden = enabled !== manifestEnabled;

  const onPickIcon = async (emoji: string) => {
    try {
      await saveBot(entry.name, { icon: emoji });
      addToast(`Icon updated for ${label}`, "success");
    } catch (e) {
      addToast(e instanceof Error ? e.message : "Failed to save icon", "error");
    }
  };

  const onToggle = async () => {
    setBusy(true);
    try {
      await setOverlay(entry.name, !enabled);
      addToast(
        !enabled ? `${label} enabled — exposed to Nexie` : `${label} disabled — hidden from Nexie`,
        !enabled ? "success" : "info",
      );
    } catch (e) {
      addToast(e instanceof Error ? e.message : `Failed to update ${label}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const onReset = async () => {
    setBusy(true);
    try {
      await setOverlay(entry.name, null);
      addToast(`${label} follows its manifest default again`, "info");
    } catch (e) {
      addToast(e instanceof Error ? e.message : `Failed to reset ${label}`, "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <div className="flex items-start gap-3">
        {entry.is_bundle ? (
          <EmojiPicker
            onSelect={(emoji) => void onPickIcon(emoji)}
            trigger={
              <button
                type="button"
                aria-label={`Icon ${identity.emoji} — change`}
                title="Pick an emoji icon for this bot"
                className="flex h-12 w-12 shrink-0 items-center justify-center rounded-md border border-border-strong bg-surface-1 text-2xl leading-none transition-colors hover:border-accent"
              >
                {identity.emoji}
              </button>
            }
          />
        ) : (
          <span
            className="flex h-12 w-12 shrink-0 items-center justify-center text-2xl leading-none"
            aria-hidden="true"
          >
            {identity.emoji}
          </span>
        )}

        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
            <h1 className={`text-base font-semibold ${identity.color}`}>{label}</h1>
            {entry.display_name?.trim() && (
              <span className="font-mono text-caption text-fg-subtle">{entry.name}</span>
            )}
            {!entry.is_bundle && <Badge>single file</Badge>}
          </div>
          {entry.description && (
            <p className="mt-0.5 text-xs text-fg-muted">{entry.description}</p>
          )}
          <div className="mt-1.5 flex flex-wrap gap-1">
            {entry.version && <Chip>v{entry.version}</Chip>}
            {entry.author && <Chip>by {entry.author}</Chip>}
          </div>
        </div>

        <div className="flex shrink-0 flex-col items-end gap-1">
          <Button
            variant={enabled ? "success" : "secondary"}
            size="sm"
            role="switch"
            aria-checked={enabled}
            disabled={busy}
            onClick={() => void onToggle()}
            title={enabled ? "Disable (hide from Nexie)" : "Enable (expose to Nexie)"}
          >
            {enabled ? "Enabled" : "Disabled"}
          </Button>
          <span className="text-caption text-fg-subtle">
            Default from manifest: {manifestEnabled ? "On" : "Off"}
          </span>
          {overridden && (
            <button
              type="button"
              onClick={() => void onReset()}
              disabled={busy}
              className="text-caption text-accent-text hover:underline"
            >
              Reset to default
            </button>
          )}
        </div>
      </div>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Actions row — Launch / Open in editor / Test
// ---------------------------------------------------------------------------

function ActionsRow({
  entry,
  launchFile,
  testOpen,
  onToggleTest,
}: {
  entry: BotEntryWithSchema;
  launchFile: string | null;
  testOpen: boolean;
  onToggleTest: () => void;
}) {
  const [, setLocation] = useLocation();
  const pipelineBoardsEnabled = useServerInfoStore(
    (state) => !!state.info?.native_tracker_enabled,
  );
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const { activeRepo, enabled: repoScopeEnabled } = useActiveRepo();

  const noPathTitle =
    "The server couldn't relativise this bot's path to the workspace — launch it from its own directory instead.";

  const onLaunch = () => {
    if (!launchFile) return;
    setLocation(`/runs/new?file=${encodeURIComponent(launchFile)}`);
  };

  const onEdit = () => {
    if (!launchFile) return;
    useTabsStore.getState().openTab("editor", { file: launchFile });
    setLocation(`/editor?file=${encodeURIComponent(launchFile)}`);
  };

  // Cloud repo-context row: surfaces the manifest `repo:` requirement against
  // the sidebar's active repo, so the operator knows before clicking Launch
  // whether the target is set (or that the bot needs one).
  const repoReq = entry.repo && entry.repo.mode !== "none" ? entry.repo : null;
  const showRepoContext = isCloud && repoScopeEnabled && !!repoReq;
  const needsRepo = repoReq?.mode === "required" && !activeRepo;

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="primary"
          size="sm"
          onClick={onLaunch}
          disabled={!launchFile}
          title={launchFile ? `Configure and launch ${entry.name}` : noPathTitle}
        >
          Launch
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onClick={onEdit}
          disabled={!launchFile}
          title={launchFile ? "Open the workflow in the editor" : noPathTitle}
        >
          Open in editor
        </Button>
        {pipelineBoardsEnabled && (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => setLocation("/pipelines")}
            title="Open the global pipeline board"
          >
            Pipeline board
          </Button>
        )}
        {/* Test opens the embedded TestRunPane (contained run: commits land
            on a storage branch only) instead of navigating away. */}
        <Button
          variant={testOpen ? "primary" : "secondary"}
          size="sm"
          onClick={onToggleTest}
          disabled={!launchFile}
          aria-expanded={testOpen}
          title={
            launchFile
              ? testOpen
                ? "Close the test pane"
                : "Open an embedded test pane (contained run — commits stay on a storage branch, no merge)"
              : noPathTitle
          }
        >
          {testOpen ? "Close test" : "Test"}
        </Button>
      </div>
      {showRepoContext && (
        needsRepo ? (
          <p className="text-caption text-warning-fg">
            Needs a target repository —{" "}
            <Link href="/integrations/connect" className="text-accent-text hover:underline">
              connect one
            </Link>
            .
          </p>
        ) : activeRepo ? (
          <p className="text-caption text-fg-subtle">
            Runs on{" "}
            <span className="font-mono text-fg-muted">{activeRepo.repo_full_name}</span>
          </p>
        ) : null
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Metadata card — the existing BotMetadataForm, standalone
// ---------------------------------------------------------------------------

function MetadataCard({ entry }: { entry: BotEntryWithSchema }) {
  return (
    <Card flush>
      <SectionTitle>Metadata</SectionTitle>
      {entry.is_bundle ? (
        // key re-seeds the form draft when navigating between bots.
        <BotMetadataForm key={entry.name} bot={entry} />
      ) : (
        <p className="px-4 pb-4 text-xs text-fg-muted">
          This is a loose <code>.bot</code> file — it has no manifest.yaml to edit. Package it as a
          bundle to give it a persona, description and catalog metadata.
        </p>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Automations card — suggested manifest invocations + active triggers
// ---------------------------------------------------------------------------

function describeBoardInvocation(inv: Invocation): string {
  const states = inv.board?.to_states ?? [];
  const labels = inv.board?.all_labels ?? [];
  const stateTxt = states.length ? states.join(" / ") : "any state";
  const labelTxt = labels.length
    ? ` with label${labels.length === 1 ? "" : "s"} ${labels.join(", ")}`
    : "";
  return `When a card enters ${stateTxt}${labelTxt}`;
}

function AutomationsCard({ entry }: { entry: BotEntryWithSchema }) {
  const addToast = useUIStore((s) => s.addToast);
  const { activeRepo } = useActiveRepo();
  const [subs, setSubs] = useState<TriggerSubscription[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [adding, setAdding] = useState(false);
  // Per-invocation editable cron (schedule kinds), keyed by index.
  const [crons, setCrons] = useState<Record<number, string>>({});
  const [busyIndex, setBusyIndex] = useState<number | null>(null);
  // Per-invocation outcome note ("already enabled", explicit 400 reason).
  const [notes, setNotes] = useState<Record<number, string>>({});

  const reload = useCallback(async () => {
    try {
      const list = await listTriggers({ bot: entry.name });
      setSubs(list);
      setLoadErr(null);
    } catch (err) {
      if (err instanceof FeatureUnavailableError) {
        setUnavailable(true);
        return;
      }
      setLoadErr(errorMessage(err));
    }
  }, [entry.name]);

  useEffect(() => {
    void reload();
  }, [reload]);

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
        <Badge variant="info">schedule</Badge>
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
          <Badge>board</Badge>
          <span className="min-w-0 flex-1 text-xs text-fg-muted">
            Dispatcher board target — no card-event filter declared, nothing to subscribe.
          </span>
        </li>
      );
    }
    return (
      <li className="flex flex-wrap items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
        <Badge variant="info">board</Badge>
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
      <Badge>{inv.kind}</Badge>
      <span className="min-w-0 flex-1 text-xs text-fg-muted">
        {desc} — wire through the forge integration.
      </span>
      <Link href="/integrations" className="shrink-0 text-caption text-accent-text hover:underline">
        Integrations →
      </Link>
    </li>
  );
}

// ---------------------------------------------------------------------------
// Vars + presets cards
// ---------------------------------------------------------------------------

function VarsCard({ entry }: { entry: BotEntryWithSchema }) {
  const fields = entry.vars?.fields ?? [];
  return (
    <Card flush>
      <SectionTitle>Variables</SectionTitle>
      {fields.length === 0 ? (
        <p className="px-4 pb-4 text-xs text-fg-subtle">This workflow declares no vars.</p>
      ) : (
        <div className="overflow-x-auto pb-1">
          <Table caption={`Vars declared by ${entry.name}`}>
            <THead>
              <Th>Name</Th>
              <Th>Type</Th>
              <Th>Default</Th>
            </THead>
            <TBody>
              {fields.map((f) => (
                <Tr key={f.name}>
                  <Td className="font-mono text-fg-default">{f.name}</Td>
                  <Td className="text-fg-muted">{f.type}</Td>
                  <Td className="font-mono text-fg-muted">
                    {f.default ? literalToString(f.default) || <Em>empty</Em> : <Em>required</Em>}
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        </div>
      )}
    </Card>
  );
}

function Em({ children }: { children: React.ReactNode }) {
  return <span className="font-sans italic text-fg-subtle">{children}</span>;
}

function PresetsCard({ entry }: { entry: BotEntryWithSchema }) {
  const presets = entry.presets?.entries ?? [];
  return (
    <Card>
      <SectionTitle flush>Presets</SectionTitle>
      <ul className="mt-1 space-y-1.5">
        {presets.map((p) => {
          const valueCount = p.values?.length ?? 0;
          return (
          <li key={p.name} className="rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
            <div className="flex items-baseline gap-1.5">
              <span className="text-xs font-medium text-fg-default">
                {p.display_name?.trim() || p.name}
              </span>
              {p.display_name?.trim() && (
                <span className="font-mono text-caption text-fg-subtle">{p.name}</span>
              )}
              <span className="ml-auto text-caption text-fg-subtle">
                {valueCount} value{valueCount === 1 ? "" : "s"}
              </span>
            </div>
            {p.description && <p className="mt-0.5 text-caption text-fg-muted">{p.description}</p>}
          </li>
          );
        })}
      </ul>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Recent runs card
// ---------------------------------------------------------------------------

function RecentRunsCard({ botName }: { botName: string }) {
  const [runs, setRuns] = useState<RunSummary[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listRuns({ bot: botName, limit: 10 })
      .then((rs) => {
        if (!cancelled) setRuns(rs);
      })
      .catch((e: unknown) => {
        if (!cancelled) setErr(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [botName]);

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle flush>Recent runs</SectionTitle>
        <Link href="/runs" className="text-caption text-accent-text hover:underline">
          All runs →
        </Link>
      </div>
      {err && (
        <InlineBanner tone="danger" title="Couldn't load runs">
          {err}
        </InlineBanner>
      )}
      {runs === null && !err ? (
        <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
          <Spinner /> Loading runs…
        </div>
      ) : (runs ?? []).length === 0 && !err ? (
        <p className="py-1 text-xs text-fg-subtle">No runs yet for this bot.</p>
      ) : (
        <ul className="mt-1 divide-y divide-border-default">
          {(runs ?? []).map((r) => (
            <li key={r.id}>
              <Link
                href={`/runs/${encodeURIComponent(r.id)}`}
                className="flex items-center gap-2 rounded px-1 py-1.5 hover:bg-surface-2"
              >
                <Badge variant={STATUS_VARIANT[r.status] ?? "neutral"}>
                  {labelForStatus(r.status)}
                </Badge>
                <span className="min-w-0 flex-1 truncate text-xs text-fg-default">
                  {r.name?.trim() || r.workflow_name}
                </span>
                <span className="shrink-0 font-mono text-caption text-fg-subtle">
                  {r.id.slice(0, 8)}
                </span>
                <span className="shrink-0 text-caption text-fg-subtle">
                  {formatRelative(r.created_at)}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Secret bindings (cloud mode only)
// ---------------------------------------------------------------------------

function SecretBindingsCard({ botName }: { botName: string }) {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id;
  const [bindings, setBindings] = useState<BotSecretBinding[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (!teamID) return;
    let cancelled = false;
    listBindings(teamID, botName)
      .then((bs) => {
        if (!cancelled) setBindings(bs);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        if (e instanceof FeatureUnavailableError) return; // no binding store wired
        setErr(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [teamID, botName]);

  if (!teamID) return null;

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle flush>Secret bindings</SectionTitle>
        <Link
          href="/integrations?tab=bindings"
          className="text-caption text-accent-text hover:underline"
        >
          Manage →
        </Link>
      </div>
      {err && (
        <InlineBanner tone="danger" title="Couldn't load bindings">
          {err}
        </InlineBanner>
      )}
      {bindings === null && !err ? (
        <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
          <Spinner /> Loading bindings…
        </div>
      ) : (bindings ?? []).length === 0 && !err ? (
        <p className="py-1 text-xs text-fg-subtle">
          No secrets bound to this bot — bind one from Integrations → Bindings.
        </p>
      ) : (
        <ul className="mt-1 space-y-1">
          {(bindings ?? []).map((b) => (
            <li
              key={b.id}
              className="flex items-center gap-2 rounded-md border border-border-default bg-surface-2 px-2 py-1.5 text-xs"
            >
              <span className="font-mono text-fg-default">{b.secret_name_for_workflow}</span>
              {b.allowed_hosts && b.allowed_hosts.length > 0 && (
                <span className="text-caption text-fg-subtle">
                  hosts: {b.allowed_hosts.join(", ")}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------

function SectionTitle({ children, flush = false }: { children: React.ReactNode; flush?: boolean }) {
  return (
    <h2 className={`text-xs font-semibold text-fg-default ${flush ? "" : "px-4 pt-3 pb-1"}`}>
      {children}
    </h2>
  );
}
