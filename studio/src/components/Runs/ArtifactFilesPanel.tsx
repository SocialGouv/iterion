import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  artifactFileURL,
  downloadArtifactFile,
  fetchArtifactFile,
  listArtifactFiles,
  type ArtifactFile,
} from "@/api/runs";
import { Dialog, Popover } from "@/components/ui";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { useRunStore } from "@/store/run";
import { useDownloadsStore, type DownloadEntry } from "@/store/downloads";
import { useUIStore } from "@/store/ui";

// Events that suggest a tool just dropped a new file. Refresh on
// node_finished (write_audit_md / emit_sbom complete here) and on the
// terminal triplet (run_finished + run_failed + run_cancelled) so the
// final SBOM shows up without the operator hitting refresh.
const REFRESH_EVENTS = new Set([
  "node_finished",
  "run_finished",
  "run_failed",
  "run_cancelled",
]);

const DEBOUNCE_MS = 300;

// Content types we render inline in the preview modal. Anything else
// gets the "use Download" fallback — covers binaries the in-sandbox
// recipe might emit (zips, tarballs, sqlite dbs, …).
const TEXT_MIME_PREFIXES = ["text/", "application/json", "application/yaml", "application/xml"];

interface Props {
  runId: string | null;
}

interface PreviewState {
  // Minimal shape — populated either from an ArtifactFile (the table)
  // or a DownloadEntry (the history popover, where the file may no
  // longer be in the current run's manifest).
  path: string;
  size: number;
  loading: boolean;
  error: string | null;
  // Exactly one of textBody / blobURL is populated once loaded.
  textBody: string | null;
  blobURL: string | null;
  contentType: string;
}

// ArtifactFilesPanel surfaces the contents of runs/<id>/artifact_files
// — the per-run scratch area where in-sandbox tools (write_audit_md,
// emit_sbom, …) drop arbitrary report/SBOM/manifest files.
export default function ArtifactFilesPanel({ runId }: Props) {
  const [files, setFiles] = useState<ArtifactFile[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<PreviewState | null>(null);
  const [downloadsOpen, setDownloadsOpen] = useState(false);
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const genRef = useRef(0);
  const previewGenRef = useRef(0);

  const events = useRunStore((s) => s.events);
  const addToast = useUIStore((s) => s.addToast);
  const allDownloads = useDownloadsStore((s) => s.entries);
  const recordDownload = useDownloadsStore((s) => s.recordDownload);
  const removeDownload = useDownloadsStore((s) => s.removeDownload);
  const clearForRun = useDownloadsStore((s) => s.clearForRun);

  const runDownloads = useMemo(
    () => (runId ? allDownloads.filter((e) => e.runId === runId) : []),
    [allDownloads, runId],
  );

  const fetchNow = useCallback(() => {
    if (!runId) return;
    const myGen = ++genRef.current;
    setLoading(true);
    listArtifactFiles(runId)
      .then((res) => {
        if (myGen !== genRef.current) return;
        setFiles(res);
        setError(null);
      })
      .catch((err: unknown) => {
        if (myGen !== genRef.current) return;
        setError(err instanceof Error ? err.message : "Failed to load files");
      })
      .finally(() => {
        if (myGen !== genRef.current) return;
        setLoading(false);
      });
  }, [runId]);

  // Initial fetch + refetch on run change.
  useEffect(() => {
    if (!runId) {
      setFiles(null);
      return;
    }
    fetchNow();
  }, [runId, fetchNow]);

  // Live refresh: when new events arrive that suggest a tool wrote a
  // file, schedule a debounced refetch. Tracking the last seen seq
  // avoids re-triggering on the same event after a re-render.
  useEffect(() => {
    if (!runId || events.length === 0) return;
    let touched = false;
    for (const ev of events) {
      if (ev.seq <= lastSeenSeqRef.current) continue;
      lastSeenSeqRef.current = ev.seq;
      if (REFRESH_EVENTS.has(ev.type)) {
        touched = true;
      }
    }
    if (!touched) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(fetchNow, DEBOUNCE_MS);
    // Clear the pending timer on unmount so a panel torn down within
    // the debounce window doesn't fire fetchNow after it's gone.
    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current);
        debounceRef.current = null;
      }
    };
  }, [events, runId, fetchNow]);

  // Revoke the blob URL we created for the *previous* preview when
  // the URL value changes. Capture the URL in the closure so the
  // cleanup function frees the right one — the prior implementation
  // dereferenced `preview?.blobURL` at cleanup time, which already
  // pointed at the NEW preview because state had been committed
  // before React ran the cleanup of the old effect version.
  useEffect(() => {
    const url = preview?.blobURL;
    if (!url) return;
    return () => {
      URL.revokeObjectURL(url);
    };
  }, [preview?.blobURL]);

  const closePreview = useCallback(() => {
    setPreview(null);
  }, []);

  const openPreview = useCallback(
    (target: { path: string; size: number }) => {
      if (!runId) return;
      const myGen = ++previewGenRef.current;
      setPreview({
        path: target.path,
        size: target.size,
        loading: true,
        error: null,
        textBody: null,
        blobURL: null,
        contentType: "",
      });
      fetchArtifactFile(runId, target.path)
        .then(async ({ blob, contentType }) => {
          if (myGen !== previewGenRef.current) return;
          const isText = TEXT_MIME_PREFIXES.some((p) => contentType.startsWith(p));
          if (isText) {
            const textBody = await blob.text();
            setPreview({
              path: target.path,
              size: target.size,
              loading: false,
              error: null,
              textBody,
              blobURL: null,
              contentType,
            });
          } else {
            const blobURL = URL.createObjectURL(blob);
            setPreview({
              path: target.path,
              size: target.size,
              loading: false,
              error: null,
              textBody: null,
              blobURL,
              contentType,
            });
          }
        })
        .catch((err: unknown) => {
          if (myGen !== previewGenRef.current) return;
          setPreview({
            path: target.path,
            size: target.size,
            loading: false,
            error: err instanceof Error ? err.message : "Failed to load preview",
            textBody: null,
            blobURL: null,
            contentType: "",
          });
        });
    },
    [runId],
  );

  const triggerDownload = useCallback(
    (target: { path: string; size: number }) => {
      if (!runId) return;
      const basename = target.path.includes("/")
        ? target.path.slice(target.path.lastIndexOf("/") + 1)
        : target.path;
      downloadArtifactFile(runId, target.path)
        .then((outcome) => {
          if (outcome.cancelled) return;
          recordDownload({
            runId,
            path: target.path,
            basename,
            size: target.size,
            contentType: outcome.contentType,
            localPath: outcome.localPath,
          });
          addToast(`Downloaded ${basename}`, "success", {
            action: {
              label: "Open",
              onClick: () => openPreview(target),
            },
          });
        })
        .catch((err: unknown) => {
          addToast(
            `Download failed: ${err instanceof Error ? err.message : "unknown error"}`,
            "error",
          );
        });
    },
    [runId, recordDownload, addToast, openPreview],
  );

  if (!runId) {
    return (
      <div className="h-full flex items-center justify-center text-fg-subtle text-xs px-4">
        No active run.
      </div>
    );
  }
  if (loading && files === null) {
    return (
      <div className="h-full flex items-center justify-center text-fg-subtle text-xs px-4">
        Loading artifacts…
      </div>
    );
  }
  if (error) {
    return (
      <div className="h-full overflow-auto px-4 py-3 text-xs">
        <div className="text-fg-error">Failed to load artifacts: {error}</div>
        <button
          type="button"
          className="mt-2 text-fg-link hover:underline"
          onClick={fetchNow}
        >
          Retry
        </button>
      </div>
    );
  }

  const downloadsButton = (
    <DownloadsButton
      count={runDownloads.length}
      open={downloadsOpen}
      onOpenChange={setDownloadsOpen}
      entries={runDownloads}
      onShow={(e) => {
        setDownloadsOpen(false);
        openPreview({ path: e.path, size: e.size });
      }}
      onRedownload={(e) => {
        setDownloadsOpen(false);
        triggerDownload({ path: e.path, size: e.size });
      }}
      onReveal={(e) => {
        if (!e.localPath) return;
        desktop.revealInFinder(e.localPath).catch((err: unknown) => {
          addToast(
            `Reveal failed: ${err instanceof Error ? err.message : "unknown error"}`,
            "error",
          );
        });
      }}
      onRemove={(e) => removeDownload(e.id)}
      onClearAll={() => clearForRun(runId)}
    />
  );

  return (
    <>
      <div className="h-full flex flex-col text-xs">
        <div className="flex items-center justify-between border-b border-border-subtle px-3 py-1.5 text-fg-subtle">
          <span>
            {(files ?? []).length} file{(files ?? []).length === 1 ? "" : "s"}
          </span>
          {downloadsButton}
        </div>
        {!files || files.length === 0 ? (
          <div className="flex-1 flex flex-col items-center justify-center text-fg-subtle px-4 text-center gap-1">
            <div>No artifact files yet.</div>
            <div className="opacity-70">
              In-sandbox tools writing into{" "}
              <code className="px-1 rounded bg-surface-2">
                $ITERION_ARTIFACT_FILES_DIR
              </code>{" "}
              appear here as they land.
            </div>
          </div>
        ) : (
          <div className="flex-1 overflow-auto">
            <table className="w-full">
              <thead className="sticky top-0 bg-surface-1 border-b border-border-default">
                <tr className="text-left text-fg-subtle">
                  <th className="px-3 py-2 font-normal">Path</th>
                  <th className="px-3 py-2 font-normal text-right whitespace-nowrap">
                    Size
                  </th>
                  <th className="px-3 py-2 font-normal whitespace-nowrap">
                    Modified
                  </th>
                  <th className="px-3 py-2 font-normal text-right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {files.map((f) => (
                  <tr
                    key={f.path}
                    className="border-b border-border-subtle hover:bg-surface-2"
                  >
                    <td className="px-3 py-1.5 font-mono">
                      <a
                        href={artifactFileURL(runId, f.path)}
                        onClick={(e) => {
                          if (e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) return;
                          e.preventDefault();
                          openPreview(f);
                        }}
                        className="text-fg-link hover:underline"
                      >
                        {f.path}
                      </a>
                    </td>
                    <td className="px-3 py-1.5 text-right text-fg-subtle whitespace-nowrap">
                      {formatSize(f.size)}
                    </td>
                    <td className="px-3 py-1.5 text-fg-subtle whitespace-nowrap">
                      {formatModified(f.modified_at)}
                    </td>
                    <td className="px-3 py-1.5 text-right whitespace-nowrap">
                      <button
                        type="button"
                        className="text-fg-link hover:underline mr-3"
                        onClick={() => triggerDownload(f)}
                      >
                        Download
                      </button>
                      <button
                        type="button"
                        className="text-fg-link hover:underline"
                        onClick={() => openPreview(f)}
                      >
                        Open
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {preview && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) closePreview();
          }}
          widthClass="max-w-4xl"
          title={
            <span className="font-mono text-xs">{preview.path}</span>
          }
          description={
            <span>
              {formatSize(preview.size)} · {preview.contentType || "loading…"}
            </span>
          }
          footer={
            <button
              type="button"
              onClick={() => triggerDownload({ path: preview.path, size: preview.size })}
              className="px-3 py-1.5 text-xs rounded border border-border-default hover:bg-surface-2"
            >
              Download
            </button>
          }
        >
          <PreviewBody preview={preview} />
        </Dialog>
      )}
    </>
  );
}

interface DownloadsButtonProps {
  count: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  entries: DownloadEntry[];
  onShow: (entry: DownloadEntry) => void;
  onRedownload: (entry: DownloadEntry) => void;
  onReveal: (entry: DownloadEntry) => void;
  onRemove: (entry: DownloadEntry) => void;
  onClearAll: () => void;
}

function DownloadsButton({
  count,
  open,
  onOpenChange,
  entries,
  onShow,
  onRedownload,
  onReveal,
  onRemove,
  onClearAll,
}: DownloadsButtonProps) {
  const desktopMode = isDesktop();
  return (
    <Popover
      open={open}
      onOpenChange={onOpenChange}
      side="bottom"
      align="end"
      contentClassName="w-[28rem] max-h-[60vh] flex flex-col"
      trigger={
        <button
          type="button"
          className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-border-subtle hover:bg-surface-2 text-fg-default"
          title="Downloads"
        >
          <span>Downloads</span>
          <span className="ml-1 inline-flex items-center justify-center min-w-[1.25rem] h-4 px-1 rounded bg-surface-2 text-fg-subtle text-[10px] font-mono">
            {count}
          </span>
        </button>
      }
    >
      <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
        <span className="text-xs font-semibold">Downloads from this run</span>
        <button
          type="button"
          disabled={entries.length === 0}
          onClick={onClearAll}
          className="text-[11px] text-fg-link hover:underline disabled:text-fg-subtle disabled:no-underline"
        >
          Clear all
        </button>
      </div>
      {entries.length === 0 ? (
        <div className="px-3 py-6 text-center text-xs text-fg-subtle">
          No downloads from this run yet.
        </div>
      ) : (
        <div className="flex-1 overflow-auto">
          <ul className="divide-y divide-border-subtle">
            {entries.map((e) => (
              <li key={e.id} className="px-3 py-2 hover:bg-surface-2">
                <div className="flex items-baseline justify-between gap-2">
                  <button
                    type="button"
                    onClick={() => onShow(e)}
                    className="font-mono text-xs text-fg-link hover:underline truncate text-left"
                    title={e.path}
                  >
                    {e.basename}
                  </button>
                  <span className="text-[10px] text-fg-subtle whitespace-nowrap">
                    {formatModified(new Date(e.downloadedAt).toISOString())}
                  </span>
                </div>
                <div className="text-[11px] text-fg-subtle truncate" title={e.localPath ?? e.path}>
                  {e.localPath ?? e.path} · {formatSize(e.size)}
                </div>
                <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-[11px]">
                  <button
                    type="button"
                    onClick={() => onShow(e)}
                    className="text-fg-link hover:underline"
                  >
                    Open
                  </button>
                  <button
                    type="button"
                    onClick={() => onRedownload(e)}
                    className="text-fg-link hover:underline"
                  >
                    Re-download
                  </button>
                  {desktopMode && e.localPath && (
                    <button
                      type="button"
                      onClick={() => onReveal(e)}
                      className="text-fg-link hover:underline"
                    >
                      Reveal in folder
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => onRemove(e)}
                    className="text-fg-subtle hover:underline ml-auto"
                  >
                    Remove
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}
    </Popover>
  );
}

function PreviewBody({ preview }: { preview: PreviewState }) {
  if (preview.loading) {
    return (
      <div className="h-64 flex items-center justify-center text-fg-subtle text-xs">
        Loading preview…
      </div>
    );
  }
  if (preview.error) {
    return (
      <div className="text-xs text-fg-error">Failed to load: {preview.error}</div>
    );
  }
  if (preview.textBody !== null) {
    return (
      <pre className="max-h-[70vh] overflow-auto text-xs font-mono whitespace-pre-wrap break-words bg-surface-0 p-3 rounded">
        {preview.textBody}
      </pre>
    );
  }
  if (preview.blobURL && preview.contentType.startsWith("image/")) {
    return (
      <div className="max-h-[70vh] overflow-auto flex items-center justify-center bg-surface-0 p-3 rounded">
        <img src={preview.blobURL} alt={preview.path} className="max-w-full" />
      </div>
    );
  }
  return (
    <div className="text-xs text-fg-subtle py-6 text-center">
      Preview not available for this file type. Use Download to save it.
    </div>
  );
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
}

function formatModified(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
