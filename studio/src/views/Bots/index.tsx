import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useLocation } from "wouter";

import type { BotEntryWithSchema, ImportScriptResult } from "@/api/bots";
import { importBotScript } from "@/api/bots";
import {
  listTriggers,
  FeatureUnavailableError,
  type TriggerSubscription,
} from "@/api/triggers";
import {
  importBotFromRepo,
  importBotzFile,
  importSuccessMessage,
} from "@/components/Catalog/importActions";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import {
  Badge,
  Button,
  Dialog,
  DropdownMenu,
  DropdownMenuItem,
  EmptyState,
  InlineBanner,
  Input,
  Spinner,
} from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { botVisual } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

/**
 * BotsView — the /bots gallery: every discovered bot as a card
 * (identity, enabled state, invocation kinds, presets + trigger counts),
 * with client-side search, import (.botz / git repo), and the entry to
 * the builder (/bots/new). Card click → the bot's home page.
 */
export default function BotsView() {
  const [, setLocation] = useLocation();
  const bots = useBotsStore((s) => s.bots);
  const loading = useBotsStore((s) => s.loading);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  const refetch = useBotsStore((s) => s.refetch);
  const addToast = useUIStore((s) => s.addToast);
  // Cloud servers refuse bot upload/install/create (403) — the catalog
  // there is git-managed. Swap the local import/builder affordances for
  // the one entry point that works: the marketplace.
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const marketplaceEnabled = useServerInfoStore(
    (s) => s.info?.marketplace_enabled === true,
  );
  const triggersEnabled = useServerInfoStore((s) => s.info?.triggers_enabled === true);

  const [query, setQuery] = useState("");

  const [uploadingBotz, setUploadingBotz] = useState(false);
  const botzFileRef = useRef<HTMLInputElement | null>(null);
  const [repoDialogOpen, setRepoDialogOpen] = useState(false);
  const scriptFileRef = useRef<HTMLInputElement | null>(null);
  // Picked workflow script awaiting preview/save (null = dialog closed).
  const [scriptImport, setScriptImport] = useState<{
    filename: string;
    source: string;
  } | null>(null);

  useEffect(() => {
    if (bots === null) void fetchBots();
    else void refetch();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Trigger counts per bot_id. null = not loaded (or the trigger store
  // isn't wired on this server — the badge is simply hidden). Gated on the
  // server advertising a trigger store, skipping the round-trip (and its
  // console 404) otherwise.
  const triggersQuery = useQuery<TriggerSubscription[]>({
    queryKey: ["triggers"],
    queryFn: () => listTriggers(),
    enabled: triggersEnabled,
  });
  const triggerCounts = useMemo(() => {
    if (!triggersQuery.data) return null;
    const counts: Record<string, number> = {};
    for (const s of triggersQuery.data) counts[s.bot_id] = (counts[s.bot_id] ?? 0) + 1;
    return counts;
  }, [triggersQuery.data]);
  // No trigger store on this server (FeatureUnavailableError) — not an
  // error, just no badge.
  const triggersError =
    triggersQuery.error && !(triggersQuery.error instanceof FeatureUnavailableError)
      ? errorMessage(triggersQuery.error)
      : null;

  const onUploadBotz = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ""; // allow re-selecting the same file
    if (!file) return;
    setUploadingBotz(true);
    try {
      const res = await importBotzFile(file);
      addToast(importSuccessMessage(res), "success");
      await refetch();
    } catch (err) {
      addToast(err instanceof Error ? err.message : "Import failed", "error");
    } finally {
      setUploadingBotz(false);
    }
  };

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Bots</span>,
    right: (
      <div className="flex items-center gap-2">
        {cloud ? (
          marketplaceEnabled && (
            <Button
              variant="primary"
              size="sm"
              onClick={() => setLocation("/marketplace")}
            >
              Browse marketplace
            </Button>
          )
        ) : (
          <>
            <DropdownMenu
              trigger={
                <Button variant="secondary" size="sm" disabled={uploadingBotz} loading={uploadingBotz}>
                  {uploadingBotz ? "Importing…" : "Import"}
                </Button>
              }
              align="end"
            >
              <DropdownMenuItem onSelect={() => botzFileRef.current?.click()}>
                From a .botz file…
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setRepoDialogOpen(true)}>
                From a git repository…
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => scriptFileRef.current?.click()}>
                From a workflow script (.js)…
              </DropdownMenuItem>
            </DropdownMenu>
            <Button variant="primary" size="sm" onClick={() => setLocation("/bots/new")}>
              New bot
            </Button>
          </>
        )}
      </div>
    ),
  });

  const rows = useMemo(() => {
    const all = bots ?? [];
    const q = query.trim().toLowerCase();
    if (!q) return all;
    return all.filter((b) =>
      [b.name, b.display_name ?? "", b.description ?? ""].some((s) =>
        s.toLowerCase().includes(q),
      ),
    );
  }, [bots, query]);

  return (
    <div className="flex flex-col gap-3 p-4">
      <input
        ref={botzFileRef}
        type="file"
        accept=".botz"
        className="hidden"
        onChange={(e) => void onUploadBotz(e)}
      />
      <input
        ref={scriptFileRef}
        type="file"
        accept=".js,.mjs"
        className="hidden"
        onChange={(e) => {
          const file = e.target.files?.[0];
          e.target.value = "";
          if (!file) return;
          void file.text().then((source) => {
            setScriptImport({ filename: file.name, source });
          });
        }}
      />

      <div className="flex items-center gap-2">
        <Input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search bots…"
          aria-label="Search bots"
          size="md"
          className="max-w-xs"
        />
        {bots !== null && (
          <span className="text-caption text-fg-subtle">
            {rows.length}/{bots.length} bot{bots.length === 1 ? "" : "s"}
          </span>
        )}
      </div>

      {botsError && (
        <InlineBanner tone="danger" title="Couldn't load bots">
          {botsError}
        </InlineBanner>
      )}
      {triggersError && (
        <InlineBanner tone="warning" title="Couldn't load trigger counts">
          {triggersError}
        </InlineBanner>
      )}

      {loading && bots === null ? (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading bots…
        </div>
      ) : rows.length === 0 && !botsError ? (
        <EmptyState
          title={query ? "No bots match your search" : "No bots discovered in this workspace"}
          message={
            query
              ? "Try a different name or description."
              : cloud
                ? "This server's catalog is git-managed. Browse the marketplace to find bots to run."
                : "Create one with the builder, or import a bundle from a .botz file or a git repository."
          }
          action={
            !query ? (
              cloud ? (
                marketplaceEnabled ? (
                  <Button
                    variant="primary"
                    size="sm"
                    onClick={() => setLocation("/marketplace")}
                  >
                    Browse marketplace
                  </Button>
                ) : undefined
              ) : (
                <Button variant="primary" size="sm" onClick={() => setLocation("/bots/new")}>
                  New bot
                </Button>
              )
            ) : undefined
          }
        />
      ) : (
        <ul className="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(260px,1fr))]">
          {rows.map((b) => (
            <BotCard
              key={b.name}
              bot={b}
              triggerCount={triggerCounts?.[b.name] ?? 0}
              onOpen={() => setLocation(`/bots/${encodeURIComponent(b.name)}`)}
            />
          ))}
        </ul>
      )}

      <RepoImportDialog
        open={repoDialogOpen}
        onOpenChange={setRepoDialogOpen}
        onImported={() => void refetch()}
      />
      {scriptImport && (
        <ScriptImportDialog
          filename={scriptImport.filename}
          source={scriptImport.source}
          onClose={() => setScriptImport(null)}
          onImported={() => void refetch()}
        />
      )}
    </div>
  );
}

function BotCard({
  bot,
  triggerCount,
  onOpen,
}: {
  bot: BotEntryWithSchema;
  triggerCount: number;
  onOpen: () => void;
}) {
  const identity = botVisual(bot);
  const label = bot.display_name?.trim() || bot.name;
  const enabled = bot.enabled !== false;
  const kinds = [...new Set((bot.invocations ?? []).map((i) => i.kind))];
  const presetCount = bot.presets?.entries?.length ?? 0;
  return (
    <li className="flex h-full flex-col rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-sm)] transition-[box-shadow,border-color,transform] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)] focus-within:border-border-strong">
      <button
        type="button"
        onClick={onOpen}
        className="flex h-full flex-col items-start gap-2 rounded-[var(--radius-lg)] p-4 text-left focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
        title={`Open ${label}'s bot page`}
      >
        <div className="flex w-full items-center gap-2">
          <span className="shrink-0 text-lg leading-none" aria-hidden="true">
            {identity.emoji}
          </span>
          <span className={`min-w-0 truncate text-sm font-medium ${identity.color}`}>{label}</span>
          {bot.display_name?.trim() && (
            <span className="min-w-0 truncate font-mono text-caption text-fg-subtle">
              {bot.name}
            </span>
          )}
          <span className="ml-auto shrink-0">
            <Badge variant={enabled ? "success" : "neutral"}>
              {enabled ? "Enabled" : "Disabled"}
            </Badge>
          </span>
        </div>
        {bot.description ? (
          <p className="line-clamp-2 text-xs text-fg-muted">{bot.description}</p>
        ) : (
          <p className="text-xs italic text-fg-subtle">No description.</p>
        )}
        <div className="mt-auto flex flex-wrap items-center gap-1">
          {kinds.map((k) => (
            <Badge key={k} variant="info">
              {k}
            </Badge>
          ))}
          {presetCount > 0 && (
            <Badge>
              {presetCount} preset{presetCount === 1 ? "" : "s"}
            </Badge>
          )}
          {triggerCount > 0 && (
            <Badge variant="accent">
              {triggerCount} trigger{triggerCount === 1 ? "" : "s"}
            </Badge>
          )}
          {!bot.is_bundle && <Badge>single file</Badge>}
        </div>
      </button>
    </li>
  );
}

/** RepoImportDialog is the gallery's "Import from a git repository" form —
 *  the same fields as the Catalog dialog's inline variant, backed by the
 *  shared importActions. */
function RepoImportDialog({
  open,
  onOpenChange,
  onImported,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onImported: () => void;
}) {
  const addToast = useUIStore((s) => s.addToast);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [path, setPath] = useState("");
  const [importing, setImporting] = useState(false);

  const onImport = async () => {
    if (!url.trim()) return;
    setImporting(true);
    try {
      const res = await importBotFromRepo({ url, ref, path });
      addToast(importSuccessMessage(res), "success");
      setUrl("");
      setRef("");
      setPath("");
      onOpenChange(false);
      onImported();
    } catch (e) {
      addToast(e instanceof Error ? e.message : "Import failed", "error");
    } finally {
      setImporting(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Import a bot from a repository"
      description="Installs into .botz/ (git-ignored). Bots are never run automatically — inspect, then launch."
      widthClass="max-w-lg"
    >
      <div className="space-y-2">
        <Input
          type="text"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="git URL (https://… or git@…) or local path"
          aria-label="Bot repository URL or local path"
          size="md"
        />
        <div className="flex gap-2">
          <Input
            type="text"
            value={ref}
            onChange={(e) => setRef(e.target.value)}
            placeholder="ref (branch/tag, optional)"
            aria-label="Git ref (branch or tag)"
            size="md"
            className="min-w-0 flex-1"
          />
          <Input
            type="text"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="subpath or bot name (optional)"
            aria-label="Subpath or bot name within repository"
            size="md"
            className="min-w-0 flex-1"
          />
        </div>
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)} disabled={importing}>
            Cancel
          </Button>
          <Button
            variant="success"
            size="sm"
            onClick={() => void onImport()}
            disabled={importing || !url.trim()}
            loading={importing}
          >
            {importing ? "Importing…" : "Install"}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

// Cheap, non-cryptographic FNV-1a digest of the picked script for the
// react-query cache key — keying on the raw source would stringify-compare
// the whole file on every lookup.
function digest(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h = ((h ^ s.charCodeAt(i)) * 0x01000193) >>> 0;
  }
  return h;
}

/** ScriptImportDialog previews a lossy .js → draft .bot conversion
 *  (POST /api/v1/bots/import, dry-run first) and saves it into bots/ on
 *  confirm. The IMPORT REPORT is surfaced up-front: this is a porting
 *  accelerator, not a faithful translation, and the draft must be
 *  reviewed before it is run. */
function ScriptImportDialog({
  filename,
  source,
  onClose,
  onImported,
}: {
  filename: string;
  source: string;
  onClose: () => void;
  onImported: () => void;
}) {
  const addToast = useUIStore((s) => s.addToast);
  const [saving, setSaving] = useState(false);

  // Dry-run conversion preview. The dialog is mounted only while a script
  // is picked, so the query fetches exactly then; retry is off because a
  // parse failure IS the result, not a transient hiccup to smooth over.
  const previewQuery = useQuery<ImportScriptResult>({
    queryKey: ["bot-script-import-preview", filename, digest(source)],
    queryFn: () => importBotScript({ source, filename, dry_run: true }),
    retry: false,
  });
  const preview = previewQuery.data ?? null;
  const error = previewQuery.error
    ? previewQuery.error instanceof Error
      ? previewQuery.error.message
      : "Import failed"
    : null;

  const onSave = async () => {
    setSaving(true);
    try {
      const res = await importBotScript({ source, filename });
      addToast(`Imported ${res.workflow_name} → ${res.path}`, "success");
      onClose();
      onImported();
    } catch (e) {
      addToast(e instanceof Error ? e.message : "Import failed", "error");
    } finally {
      setSaving(false);
    }
  };

  const reportBlock = (label: string, entries?: string[]) =>
    entries && entries.length > 0 ? (
      <div>
        <div className="text-caption font-medium text-fg-muted">
          {label} ({entries.length})
        </div>
        <ul className="ml-3 list-disc text-caption text-fg-subtle">
          {entries.map((e, i) => (
            <li key={i} className="font-mono">
              {e}
            </li>
          ))}
        </ul>
      </div>
    ) : null;

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={`Import ${filename} as a draft bot`}
      description="Lossy conversion — the script's JS is never run. Review every IMPORT marker before launching the draft."
      widthClass="max-w-3xl"
    >
      <div className="space-y-3">
        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}
        {!error && !preview && (
          <div className="flex items-center gap-2 text-xs text-fg-muted">
            <Spinner /> Converting…
          </div>
        )}
        {preview && (
          <>
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <span className="text-fg-muted">workflow</span>
              <span className="font-mono font-medium text-fg-default">
                {preview.workflow_name}
              </span>
              {preview.needs_attention ? (
                <Badge variant="warning">needs review</Badge>
              ) : (
                <Badge variant="success">clean import</Badge>
              )}
            </div>
            <div className="grid gap-2 sm:grid-cols-2">
              {reportBlock("Mapped", preview.report.mapped)}
              {reportBlock("Holes (vars to fill)", preview.report.holes)}
              {reportBlock("Placeholders", preview.report.placeholders)}
              {reportBlock("Dropped", preview.report.dropped)}
            </div>
            <div>
              <div className="text-caption font-medium text-fg-muted">Draft preview</div>
              <pre className="max-h-72 overflow-auto rounded border border-border-subtle bg-surface-0 p-2 text-caption leading-snug">
                <code>{preview.bot_source}</code>
              </pre>
            </div>
          </>
        )}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" size="sm" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button
            variant="success"
            size="sm"
            onClick={() => void onSave()}
            disabled={saving || !preview}
            loading={saving}
          >
            {saving ? "Saving…" : "Save draft to bots/"}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
