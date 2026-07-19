import { useCallback, useEffect, useState } from "react";
import { useLocation } from "wouter";
import { useQuery } from "@tanstack/react-query";
import {
  FilePlusIcon,
  FileIcon,
  TrashIcon,
  ChevronDownIcon,
  ChevronRightIcon,
} from "@radix-ui/react-icons";

import * as api from "@/api/client";
import { getOrCreateDocumentStore } from "@/store/document";
import { openExampleIntoStore } from "@/lib/openExample";
import { useRecentsStore } from "@/store/recents";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { basename } from "@/lib/format";
import { botIdentity } from "@/lib/personas";
import { Button, IconButton, InlineBanner } from "@/components/ui";
import { EmptyState } from "@/components/ui/EmptyState";
import { BotCatalogDialog } from "@/components/Catalog/BotCatalogDialog";

// Stable empty fallback so the undefined→loaded transition doesn't
// hand the render a fresh [] reference each pass.
const EMPTY_EXAMPLES: api.ExampleEntry[] = [];

type Variant = "card" | "plain";

interface Props {
  // "card" (default): bordered section with header, used on Home.
  // "plain": no chrome, used as the EditorTabsView empty state where
  // the host already provides a centered container.
  variant?: Variant;
}

// RecentFilesPanel (a.k.a. workflow picker) shows the three ways to
// start an editor tab: a fresh blank workflow, one of the bundled
// examples, or one of the user's recent files. Every entry point
// opens (or focuses) an editor tab via the tabs store — never the
// singleton document store — so multi-file editing stays consistent.
export default function RecentFilesPanel({ variant = "card" }: Props) {
  const [, setLocation] = useLocation();
  const recents = useRecentsStore((s) => s.recents);
  const pushRecent = useRecentsStore((s) => s.pushRecent);
  const removeRecent = useRecentsStore((s) => s.removeRecent);
  const clearRecents = useRecentsStore((s) => s.clearRecents);
  const addToast = useUIStore((s) => s.addToast);

  const [examplesOpen, setExamplesOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [catalogOpen, setCatalogOpen] = useState(false);
  const { confirm, dialog: confirmDialog } = useConfirm();

  // First-class bots are static for the lifetime of the server process →
  // staleTime: Infinity, so landing on / never refetches once the shared
  // ["example-entries"] cache is warm (FilePicker and the canvas
  // ExamplesPicker refresh the same cache when opened).
  const examplesQuery = useQuery({
    queryKey: ["example-entries"],
    queryFn: () => api.listExampleEntries(),
    staleTime: Infinity,
  });
  const examples = examplesQuery.data ?? EMPTY_EXAMPLES;
  // Quietly degrade on error — the examples list is only one of three
  // entry points (recents + new blank stay reachable). The inline banner
  // below tells the operator the bot catalog couldn't load.
  const examplesError = examplesQuery.error
    ? errorMessage(examplesQuery.error)
    : null;

  useEffect(() => {
    if (recents.length === 0 && examples.length > 0) setExamplesOpen(true);
  }, [recents.length, examples.length]);

  const handleNewBlank = useCallback(() => {
    useTabsStore.getState().newEditorTab();
    setLocation("/editor");
  }, [setLocation]);

  const handleOpenRecent = useCallback(
    (path: string) => {
      // Opening a recent file routes through openTab — EditorTabHost
      // will fetch the document via api.openFile on mount and bind it
      // to the per-tab store. Re-clicking the same path focuses the
      // existing tab instead of reloading.
      useTabsStore.getState().openTab("editor", { file: path });
      pushRecent(path);
      setLocation(`/editor?file=${encodeURIComponent(path)}`);
    },
    [pushRecent, setLocation],
  );

  const handleOpenExample = useCallback(
    async (name: string) => {
      setBusy(true);
      // Open the bot in a fresh editor tab. Create the tab first so its
      // per-tab document store exists, then load + apply via the shared
      // helper (binds bots/<name> so Run enables, keeps source/diagnostics,
      // marks the loaded state as the clean baseline). On failure, close
      // the empty tab so a load error doesn't strand an untitled tab.
      const tabId = useTabsStore.getState().newEditorTab(name);
      try {
        await openExampleIntoStore(name, getOrCreateDocumentStore(tabId).getState());
        setLocation("/editor");
      } catch {
        useTabsStore.getState().closeTab(tabId);
        addToast("Failed to open example", "error");
      } finally {
        setBusy(false);
      }
    },
    [addToast, setLocation],
  );

  const body = (
    <div className={variant === "card" ? "p-3 space-y-3" : "space-y-3"}>
      <Button
        onClick={handleNewBlank}
        disabled={busy}
        variant="primary"
        size="md"
        leadingIcon={<FilePlusIcon className="w-4 h-4" />}
        className="w-full justify-start"
      >
        New blank workflow
      </Button>
      {examplesError && (
        <InlineBanner
          tone="warning"
          layout="inline"
          title="Bot catalog unavailable"
        >
          {examplesError}
        </InlineBanner>
      )}

      {examples.length > 0 && (
        <div>
          <div className="flex items-center justify-between px-1">
            <button
              type="button"
              onClick={() => setExamplesOpen((v) => !v)}
              className="flex items-center gap-1 text-xs font-medium text-fg-muted hover:text-fg-default focus-visible:ring-1 focus-visible:ring-accent rounded"
              aria-expanded={examplesOpen}
            >
              {examplesOpen ? (
                <ChevronDownIcon className="w-3 h-3" />
              ) : (
                <ChevronRightIcon className="w-3 h-3" />
              )}
              <span>Bots ({examples.length})</span>
            </button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setCatalogOpen(true)}
              title="Enable/disable bots and edit their metadata"
            >
              Manage
            </Button>
          </div>
          {examplesOpen && (
            <ul className="mt-1 space-y-0.5">
              {examples.map((ex) => {
                // Persona (manifest display_name) + emoji on line 1, with
                // the technical name (the first path segment, e.g.
                // "whats-next") muted beside it and a one-line description
                // below. The technical id also drives the emoji/colour
                // lookup. Falls back to the raw name for an embedded
                // recipe with no on-disk persona.
                const techName = ex.name.split("/")[0];
                const identity = botIdentity(techName);
                return (
                  <li key={ex.name}>
                    <button
                      type="button"
                      onClick={() => handleOpenExample(ex.name)}
                      disabled={busy}
                      className="w-full flex items-start gap-2 px-2 py-1.5 rounded hover:bg-surface-2 text-left disabled:opacity-50 focus-visible:ring-1 focus-visible:ring-accent"
                    >
                      <span
                        className="text-sm leading-none shrink-0 mt-0.5"
                        aria-hidden="true"
                      >
                        {identity.emoji}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span className="flex items-baseline gap-1.5">
                          <span
                            className={`truncate font-medium text-xs ${identity.color}`}
                          >
                            {ex.display_name || ex.name}
                          </span>
                          {ex.display_name && (
                            <span className="font-mono text-fg-subtle text-caption truncate shrink-0">
                              {techName}
                            </span>
                          )}
                        </span>
                        {ex.description && (
                          <span className="block truncate text-fg-muted text-micro mt-0.5">
                            {ex.description}
                          </span>
                        )}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}

      <div>
        <div className="flex items-center justify-between px-1">
          <span className="text-xs font-medium text-fg-muted">
            Recent ({recents.length})
          </span>
          {recents.length > 0 && (
            <Button
              variant="ghost"
              size="sm"
              onClick={async () => {
                const ok = await confirm({
                  title: "Clear recent files?",
                  message:
                    "All entries in the Recent list will be removed. Your files on disk are not affected.",
                  confirmLabel: "Clear",
                  confirmVariant: "danger",
                });
                if (ok) clearRecents();
              }}
            >
              Clear all
            </Button>
          )}
        </div>
        {recents.length === 0 ? (
          <EmptyState
            className="mt-2 py-3"
            message={
              examples.length > 0
                ? "No recent files yet — start from an example above or create a new workflow."
                : "No recent files yet — create a new workflow."
            }
          />
        ) : (
          <ul className="mt-1 space-y-0.5">
            {recents.map((path) => (
              <li key={path} className="group flex items-center gap-1">
                <button
                  type="button"
                  onClick={() => handleOpenRecent(path)}
                  disabled={busy}
                  className="flex-1 min-w-0 flex items-center gap-2 px-2 py-1.5 rounded hover:bg-surface-2 text-left text-xs disabled:opacity-50 focus-visible:ring-1 focus-visible:ring-accent"
                  title={path}
                >
                  <FileIcon className="w-3.5 h-3.5 text-fg-subtle shrink-0" />
                  <span className="font-medium truncate">
                    {basename(path)}
                  </span>
                  {basename(path) !== path && (
                    <span className="text-fg-subtle text-caption truncate">
                      {path}
                    </span>
                  )}
                </button>
                <IconButton
                  label={`Remove ${path} from recents`}
                  tooltip="Remove from recents"
                  size="sm"
                  variant="ghost"
                  onClick={(e) => {
                    e.stopPropagation();
                    removeRecent(path);
                  }}
                  className="h-7 w-7 text-fg-subtle hover:text-danger opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity"
                >
                  <TrashIcon className="w-3.5 h-3.5" />
                </IconButton>
              </li>
            ))}
          </ul>
        )}
      </div>
      {confirmDialog}
      <BotCatalogDialog open={catalogOpen} onOpenChange={setCatalogOpen} />
    </div>
  );

  if (variant === "plain") return body;

  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Workflows
        </h2>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setCatalogOpen(true)}
          title="Enable/disable bots and edit their metadata"
        >
          Manage bots
        </Button>
      </header>
      {body}
    </section>
  );
}
