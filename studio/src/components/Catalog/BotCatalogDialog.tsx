import { useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  importBotFromRepo,
  importBotzFile,
  importSuccessMessage,
} from "@/components/Catalog/importActions";
import { useAuth } from "@/auth/AuthContext";
import { Button, Dialog, Input } from "@/components/ui";
import { botVisual } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";

// bundleMainRel returns the workspace-relative main.bot path the editor's
// openFile expects, using the server-provided rel_path. Null when the bot
// isn't an editable bundle or the server couldn't relativise it.
function bundleMainRel(b: BotEntryWithSchema): string | null {
  return b.is_bundle && b.rel_path ? `${b.rel_path}/main.bot` : null;
}

/**
 * BotCatalogDialog is the catalog manager: every discovered bot with an
 * instant enable/disable toggle (workspace overlay — no manifest/git
 * churn) and a jump-to-edit affordance that opens the bot's main.bot in
 * the editor with the Bot metadata tab focused. Disabled bots are shown
 * desaturated so they can be flipped back on.
 */
export function BotCatalogDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [, setLocation] = useLocation();
  const bots = useBotsStore((s) => s.bots);
  const loading = useBotsStore((s) => s.loading);
  const fetchBots = useBotsStore((s) => s.fetch);
  const refetch = useBotsStore((s) => s.refetch);
  const setOverlay = useBotsStore((s) => s.setOverlay);
  const addToast = useUIStore((s) => s.addToast);
  const setActiveTab = useUIStore((s) => s.setActiveTab);
  const { activeTeam } = useAuth();
  const info = useServerInfoStore((s) => s.info);
  // Forge integrations are cloud-only and team-scoped, so the "Connect to a
  // repo" affordance only applies when a team is active in cloud mode.
  const canConnectRepo = !!activeTeam && info?.mode === "cloud";
  const [busy, setBusy] = useState<string | null>(null);
  const [showImport, setShowImport] = useState(false);
  const [importUrl, setImportUrl] = useState("");
  const [importRef, setImportRef] = useState("");
  const [importPath, setImportPath] = useState("");
  const [importing, setImporting] = useState(false);
  const [uploadingBotz, setUploadingBotz] = useState(false);
  const botzFileRef = useRef<HTMLInputElement | null>(null);

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

  const onImport = async () => {
    const url = importUrl.trim();
    if (!url) return;
    setImporting(true);
    try {
      const res = await importBotFromRepo({ url, ref: importRef, path: importPath });
      addToast(importSuccessMessage(res), "success");
      setImportUrl("");
      setImportRef("");
      setImportPath("");
      setShowImport(false);
      await refetch();
    } catch (e) {
      addToast(e instanceof Error ? e.message : "Import failed", "error");
    } finally {
      setImporting(false);
    }
  };

  useEffect(() => {
    if (!open) return;
    if (bots === null) void fetchBots();
    else void refetch();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const onToggle = async (b: BotEntryWithSchema) => {
    const next = b.enabled === false; // flipping to this state
    const label = b.display_name?.trim() || b.name;
    setBusy(b.name);
    try {
      await setOverlay(b.name, next);
      addToast(
        next ? `${label} enabled — exposed to Nexie` : `${label} disabled — hidden from Nexie`,
        next ? "success" : "info",
      );
    } catch (e) {
      addToast(e instanceof Error ? e.message : `Failed to update ${label}`, "error");
    } finally {
      setBusy(null);
    }
  };

  const onEdit = (b: BotEntryWithSchema) => {
    const rel = bundleMainRel(b);
    if (!rel) return;
    useTabsStore.getState().openTab("editor", { file: rel });
    setActiveTab("bot");
    onOpenChange(false);
    setLocation(`/editor?file=${encodeURIComponent(rel)}`);
  };

  // onConnect jumps to the Integrations page's Forges tab to connect a forge +
  // enable this bot on a repo. The ?bot= hint pre-checks it in the enable dialog
  // (and auto-opens that dialog when there's a single connection).
  const onConnect = (botName: string) => {
    if (!activeTeam) return;
    onOpenChange(false);
    setLocation(`/integrations?tab=forges&bot=${encodeURIComponent(botName)}`);
  };

  const rows = bots ?? [];

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Manage bots"
      description="Toggle which bots are exposed to Nexie and the board picker. Toggles are workspace-local (they don't edit the bot's manifest)."
      widthClass="max-w-2xl"
    >
      {/* Import/create is a local-studio affordance — the cloud server
          403s upload/install/create, and the marketplace is the real
          entry point there (mirrors the /bots gallery header). */}
      {info?.mode === "cloud" ? (
        <div className="mb-3 flex items-center justify-between gap-3 border-b border-border-default pb-3">
          <p className="text-caption text-fg-subtle">
            This server&apos;s catalog is git-managed — find more bots on the
            marketplace.
          </p>
          {info?.marketplace_enabled && (
            <Button
              variant="secondary"
              size="sm"
              className="shrink-0"
              onClick={() => {
                onOpenChange(false);
                setLocation("/marketplace");
              }}
            >
              Browse marketplace
            </Button>
          )}
        </div>
      ) : (
      <div className="mb-3 border-b border-border-default pb-3">
        <div className="flex items-center justify-between">
          <h3 className="text-xs font-medium text-fg-muted">
            Import a bot from a repository or a <code className="text-fg-default">.botz</code> file
          </h3>
          <div className="flex items-center gap-2">
            <Button
              variant="primary"
              size="sm"
              onClick={() => {
                onOpenChange(false);
                setLocation("/bots/new");
              }}
            >
              New bot
            </Button>
            <input
              ref={botzFileRef}
              type="file"
              accept=".botz"
              className="hidden"
              onChange={(e) => void onUploadBotz(e)}
            />
            <Button
              variant="secondary"
              size="sm"
              onClick={() => botzFileRef.current?.click()}
              disabled={uploadingBotz}
              loading={uploadingBotz}
            >
              {uploadingBotz ? "Importing…" : "Import .botz…"}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setShowImport((v) => !v)}
              aria-expanded={showImport}
            >
              {showImport ? "Cancel" : "Import from repo…"}
            </Button>
          </div>
        </div>
        {showImport && (
          <div className="mt-2 space-y-2">
            <Input
              type="text"
              value={importUrl}
              onChange={(e) => setImportUrl(e.target.value)}
              placeholder="git URL (https://… or git@…) or local path"
              aria-label="Bot repository URL or local path"
              size="md"
            />
            <div className="flex gap-2">
              <Input
                type="text"
                value={importRef}
                onChange={(e) => setImportRef(e.target.value)}
                placeholder="ref (branch/tag, optional)"
                aria-label="Git ref (branch or tag)"
                size="md"
                className="min-w-0 flex-1"
              />
              <Input
                type="text"
                value={importPath}
                onChange={(e) => setImportPath(e.target.value)}
                placeholder="subpath or bot name (optional)"
                aria-label="Subpath or bot name within repository"
                size="md"
                className="min-w-0 flex-1"
              />
            </div>
            <div className="flex items-center justify-between gap-3">
              <p className="text-caption text-fg-subtle">
                Installs into .botz/ (git-ignored). Bots are never run
                automatically — inspect, then launch.
              </p>
              <Button
                variant="success"
                size="sm"
                onClick={() => void onImport()}
                disabled={importing || !importUrl.trim()}
                loading={importing}
                className="shrink-0"
              >
                {importing ? "Importing…" : "Install"}
              </Button>
            </div>
          </div>
        )}
      </div>
      )}
      <div className="max-h-[60vh] overflow-y-auto">
        {loading && rows.length === 0 && (
          <p className="px-1 py-4 text-sm text-fg-subtle">Loading bots…</p>
        )}
        {!loading && rows.length === 0 && (
          <p className="px-1 py-4 text-sm text-fg-subtle">No bots discovered in this workspace.</p>
        )}
        <ul className="space-y-0.5">
          {rows.map((b) => {
            const enabled = b.enabled !== false;
            const identity = botVisual(b);
            const label = b.display_name?.trim();
            const canEdit = bundleMainRel(b) !== null;
            return (
              <li
                key={b.name}
                className={`flex items-center gap-3 rounded px-2 py-2 hover:bg-surface-2 ${enabled ? "" : "opacity-60"}`}
              >
                <span className="shrink-0 text-base leading-none" aria-hidden="true">
                  {identity.emoji}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex items-baseline gap-1.5">
                    <button
                      type="button"
                      onClick={() => {
                        onOpenChange(false);
                        setLocation(`/bots/${encodeURIComponent(b.name)}`);
                      }}
                      title={`Open ${label || b.name}'s bot page`}
                      className={`truncate text-sm font-medium hover:underline focus:outline-none focus-visible:ring-1 focus-visible:ring-accent rounded ${identity.color}`}
                    >
                      {label || b.name}
                    </button>
                    {label && (
                      <span className="shrink-0 truncate font-mono text-caption text-fg-subtle">
                        {b.name}
                      </span>
                    )}
                    {!b.is_bundle && (
                      <span className="shrink-0 text-caption text-fg-subtle">(single file)</span>
                    )}
                  </div>
                  {b.description && (
                    <div className="truncate text-micro text-fg-muted">{b.description}</div>
                  )}
                </div>
                {b.forge && canConnectRepo && (
                  <Button
                    variant="primary"
                    size="sm"
                    onClick={() => onConnect(b.name)}
                    title="Connect a GitLab/GitHub/Forgejo repo and enable this bot on it"
                    className="shrink-0"
                  >
                    Connect to a repo
                  </Button>
                )}
                {canEdit && (
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => onEdit(b)}
                    className="shrink-0"
                  >
                    Edit
                  </Button>
                )}
                <Button
                  variant={enabled ? "success" : "secondary"}
                  size="sm"
                  role="switch"
                  aria-checked={enabled}
                  disabled={busy === b.name}
                  onClick={() => onToggle(b)}
                  title={enabled ? "Disable (hide from Nexie)" : "Enable (expose to Nexie)"}
                  className="shrink-0"
                >
                  {enabled ? "On" : "Off"}
                </Button>
              </li>
            );
          })}
        </ul>
      </div>
    </Dialog>
  );
}
