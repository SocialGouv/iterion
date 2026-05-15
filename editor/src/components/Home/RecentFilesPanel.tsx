import { useCallback, useEffect, useState } from "react";
import { useLocation } from "wouter";
import {
  FilePlusIcon,
  FileIcon,
  TrashIcon,
  ChevronDownIcon,
  ChevronRightIcon,
} from "@radix-ui/react-icons";

import * as api from "@/api/client";
import { useDocumentStore } from "@/store/document";
import { useRecentsStore } from "@/store/recents";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import { basename } from "@/lib/format";

// Examples are static for the lifetime of the server process, so cache
// the first successful response and reuse it on every subsequent mount.
// Avoids a network round-trip every time the user lands on /.
let examplesCache: string[] | null = null;
let examplesPromise: Promise<string[]> | null = null;

function loadExamples(): Promise<string[]> {
  if (examplesCache) return Promise.resolve(examplesCache);
  if (!examplesPromise) {
    examplesPromise = api.listExamples().then((list) => {
      examplesCache = list;
      return list;
    }).catch((err) => {
      examplesPromise = null;
      throw err;
    });
  }
  return examplesPromise;
}

export default function RecentFilesPanel() {
  const [, setLocation] = useLocation();
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const isDirty = useDocumentStore((s) => s.isDirty);
  const recents = useRecentsStore((s) => s.recents);
  const pushRecent = useRecentsStore((s) => s.pushRecent);
  const removeRecent = useRecentsStore((s) => s.removeRecent);
  const clearRecents = useRecentsStore((s) => s.clearRecents);
  const addToast = useUIStore((s) => s.addToast);

  const [examples, setExamples] = useState<string[]>([]);
  const [examplesOpen, setExamplesOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    // Errors are swallowed: examples are a nice-to-have, hiding the
    // subsection is preferable to a toast on every landing.
    loadExamples()
      .then((list) => {
        if (!cancelled) setExamples(list);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  // Expand examples by default when the user has no recents — gives
  // first-time users an obvious starting point.
  useEffect(() => {
    if (recents.length === 0 && examples.length > 0) setExamplesOpen(true);
  }, [recents.length, examples.length]);

  const confirmDiscard = useCallback(() => {
    if (!isDirty()) return true;
    return window.confirm("You have unsaved changes. Discard them?");
  }, [isDirty]);

  const handleNewBlank = useCallback(() => {
    if (!confirmDiscard()) return;
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    setCurrentSource(null);
    markSaved();
    setLocation("/editor");
  }, [
    confirmDiscard,
    setDocument,
    setDiagnostics,
    setCurrentFilePath,
    setCurrentSource,
    markSaved,
    setLocation,
  ]);

  const handleOpenRecent = useCallback(
    async (path: string) => {
      if (!confirmDiscard()) return;
      setBusy(true);
      try {
        const result = await api.openFile(path);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(result.path);
        setCurrentSource(result.source);
        pushRecent(result.path);
        markSaved();
        setLocation("/editor");
      } catch (err) {
        const message = (err as Error).message ?? "";
        const isMissing = /file not found|no such file|404/i.test(message);
        if (isMissing) {
          removeRecent(path);
          addToast(`Removed missing file from recents: ${path}`, "warning");
        } else {
          addToast("Open failed", "error");
        }
      } finally {
        setBusy(false);
      }
    },
    [
      confirmDiscard,
      setDocument,
      setDiagnostics,
      setCurrentFilePath,
      setCurrentSource,
      pushRecent,
      removeRecent,
      markSaved,
      addToast,
      setLocation,
    ],
  );

  const handleOpenExample = useCallback(
    async (name: string) => {
      if (!confirmDiscard()) return;
      setBusy(true);
      try {
        const result = await api.loadExample(name);
        setDocument(result.document);
        setDiagnostics(result.diagnostics);
        setCurrentFilePath(`examples/${name}`);
        setCurrentSource(null);
        markSaved();
        setLocation("/editor");
      } catch {
        addToast("Failed to open example", "error");
      } finally {
        setBusy(false);
      }
    },
    [
      confirmDiscard,
      setDocument,
      setDiagnostics,
      setCurrentFilePath,
      setCurrentSource,
      markSaved,
      addToast,
      setLocation,
    ],
  );

  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-lg overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Workflows
        </h2>
      </header>

      <div className="p-3 space-y-3">
        <button
          onClick={handleNewBlank}
          disabled={busy}
          className="w-full flex items-center gap-2 px-3 py-2 rounded-md bg-accent-soft hover:bg-accent/20 border border-accent/40 text-sm disabled:opacity-50"
        >
          <FilePlusIcon className="w-4 h-4" />
          <span className="font-medium">New blank workflow</span>
        </button>

        {examples.length > 0 && (
          <div>
            <button
              onClick={() => setExamplesOpen((v) => !v)}
              className="w-full flex items-center gap-1 text-xs font-medium text-fg-muted hover:text-fg-default px-1"
            >
              {examplesOpen ? (
                <ChevronDownIcon className="w-3 h-3" />
              ) : (
                <ChevronRightIcon className="w-3 h-3" />
              )}
              <span>Examples ({examples.length})</span>
            </button>
            {examplesOpen && (
              <ul className="mt-1 space-y-0.5">
                {examples.map((name) => (
                  <li key={name}>
                    <button
                      onClick={() => handleOpenExample(name)}
                      disabled={busy}
                      className="w-full flex items-center gap-2 px-2 py-1.5 rounded hover:bg-surface-2 text-left text-xs disabled:opacity-50"
                    >
                      <FileIcon className="w-3.5 h-3.5 text-fg-subtle shrink-0" />
                      <span className="truncate">{name}</span>
                    </button>
                  </li>
                ))}
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
              <button
                onClick={() => {
                  if (window.confirm("Clear all recent files?")) clearRecents();
                }}
                className="text-[10px] text-fg-subtle hover:text-fg-default"
              >
                Clear all
              </button>
            )}
          </div>
          {recents.length === 0 ? (
            <div className="mt-2 px-2 py-3 text-xs text-fg-subtle">
              {examples.length > 0
                ? "No recent files yet — start from an example above or create a new workflow."
                : "No recent files yet — create a new workflow."}
            </div>
          ) : (
            <ul className="mt-1 space-y-0.5">
              {recents.map((path) => (
                <li key={path} className="group flex items-center gap-1">
                  <button
                    onClick={() => handleOpenRecent(path)}
                    disabled={busy}
                    className="flex-1 min-w-0 flex items-center gap-2 px-2 py-1.5 rounded hover:bg-surface-2 text-left text-xs disabled:opacity-50"
                    title={path}
                  >
                    <FileIcon className="w-3.5 h-3.5 text-fg-subtle shrink-0" />
                    <span className="font-medium truncate">
                      {basename(path)}
                    </span>
                    {basename(path) !== path && (
                      <span className="text-fg-subtle text-[10px] truncate">
                        {path}
                      </span>
                    )}
                  </button>
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      removeRecent(path);
                    }}
                    className="p-1 text-fg-subtle hover:text-danger opacity-0 group-hover:opacity-100 transition-opacity"
                    title="Remove from recents"
                    aria-label={`Remove ${path} from recents`}
                  >
                    <TrashIcon className="w-3.5 h-3.5" />
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </section>
  );
}
