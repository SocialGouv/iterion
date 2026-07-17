import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
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

  const [query, setQuery] = useState("");
  // Trigger counts per bot_id. null = not loaded (or the trigger store
  // isn't wired on this server — the badge is simply hidden).
  const [triggerCounts, setTriggerCounts] = useState<Record<string, number> | null>(null);
  const [triggersError, setTriggersError] = useState<string | null>(null);

  const [uploadingBotz, setUploadingBotz] = useState(false);
  const botzFileRef = useRef<HTMLInputElement | null>(null);
  const [repoDialogOpen, setRepoDialogOpen] = useState(false);

  useEffect(() => {
    if (bots === null) void fetchBots();
    else void refetch();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    let cancelled = false;
    listTriggers()
      .then((subs: TriggerSubscription[]) => {
        if (cancelled) return;
        const counts: Record<string, number> = {};
        for (const s of subs) counts[s.bot_id] = (counts[s.bot_id] ?? 0) + 1;
        setTriggerCounts(counts);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // No trigger store on this server — not an error, just no badge.
        if (err instanceof FeatureUnavailableError) return;
        setTriggersError(errorMessage(err));
      });
    return () => {
      cancelled = true;
    };
  }, []);

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
