import { useCallback, useEffect, useRef, useState } from "react";

import {
  artifactFileURL,
  downloadArtifactFile,
  fetchArtifactFile,
  listArtifactFiles,
  type ArtifactFile,
} from "@/api/runs";
import { Dialog } from "@/components/ui";
import { useRunStore } from "@/store/run";
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
  file: ArtifactFile;
  loading: boolean;
  error: string | null;
  // Exactly one of textBody / blobURL is populated once loaded.
  textBody: string | null;
  blobURL: string | null;
  contentType: string;
}

// ArtifactFilesPanel surfaces the contents of runs/<id>/artifact_files
// — the per-run scratch area where in-sandbox tools (write_audit_md,
// emit_sbom, …) drop arbitrary report/SBOM/manifest files. This
// replaces the prior pattern of committing `docs/renovacy/*.md` into
// the bench repo (which leaked info + cluttered the operator's git
// history).
export default function ArtifactFilesPanel({ runId }: Props) {
  const [files, setFiles] = useState<ArtifactFile[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<PreviewState | null>(null);
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const genRef = useRef(0);
  const previewGenRef = useRef(0);

  const events = useRunStore((s) => s.events);
  const addToast = useUIStore((s) => s.addToast);

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
  }, [events, runId, fetchNow]);

  // Revoke any blob URL we created for a previous preview so the
  // browser can release the bytes when the modal closes or swaps to
  // another file.
  useEffect(() => {
    return () => {
      if (preview?.blobURL) URL.revokeObjectURL(preview.blobURL);
    };
  }, [preview?.blobURL]);

  const closePreview = useCallback(() => {
    setPreview(null);
  }, []);

  const openPreview = useCallback(
    (file: ArtifactFile) => {
      if (!runId) return;
      const myGen = ++previewGenRef.current;
      setPreview({
        file,
        loading: true,
        error: null,
        textBody: null,
        blobURL: null,
        contentType: "",
      });
      fetchArtifactFile(runId, file.path)
        .then(async ({ blob, contentType }) => {
          if (myGen !== previewGenRef.current) return;
          const isText = TEXT_MIME_PREFIXES.some((p) => contentType.startsWith(p));
          if (isText) {
            const textBody = await blob.text();
            setPreview({
              file,
              loading: false,
              error: null,
              textBody,
              blobURL: null,
              contentType,
            });
          } else {
            const blobURL = URL.createObjectURL(blob);
            setPreview({
              file,
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
            file,
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
    (file: ArtifactFile) => {
      if (!runId) return;
      downloadArtifactFile(runId, file.path).catch((err: unknown) => {
        addToast(
          `Download failed: ${err instanceof Error ? err.message : "unknown error"}`,
          "error",
        );
      });
    },
    [runId, addToast],
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
  if (!files || files.length === 0) {
    return (
      <div className="h-full flex flex-col items-center justify-center text-fg-subtle text-xs px-4 text-center gap-1">
        <div>No artifact files yet.</div>
        <div className="opacity-70">
          In-sandbox tools writing into{" "}
          <code className="px-1 rounded bg-surface-2">
            $ITERION_ARTIFACT_FILES_DIR
          </code>{" "}
          appear here as they land.
        </div>
      </div>
    );
  }

  return (
    <>
      <div className="h-full overflow-auto text-xs">
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
                  {/* Anchor (not button) so middle-click in browser
                      mode still opens the raw URL in a new tab — the
                      onClick handles the common left-click case
                      uniformly across browser + Wails. */}
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
                    download
                  </button>
                  <button
                    type="button"
                    className="text-fg-link hover:underline"
                    onClick={() => openPreview(f)}
                  >
                    open
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {preview && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) closePreview();
          }}
          widthClass="max-w-4xl"
          title={
            <span className="font-mono text-xs">{preview.file.path}</span>
          }
          description={
            <span>
              {formatSize(preview.file.size)} · {preview.contentType || "loading…"}
            </span>
          }
          footer={
            <button
              type="button"
              onClick={() => triggerDownload(preview.file)}
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
        <img src={preview.blobURL} alt={preview.file.path} className="max-w-full" />
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
