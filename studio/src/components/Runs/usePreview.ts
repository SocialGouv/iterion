// usePreview encapsulates the inline preview lifecycle shared by
// ArtifactFilesPanel: the generation-guarded fetch, the
// text/blob branch, the blob ObjectURL cleanup, and the open/close
// helpers. Lifted here so the panel becomes the orchestrator and
// re-uses identical preview semantics regardless of the trigger
// (table row, downloads popover, toast action).
//
// Gen-guarding: openPreview() increments a ref-counted generation so a
// stale fetch landing after a newer one is dropped. The blob URL is
// captured in the cleanup effect's closure (not dereferenced at cleanup
// time) so we revoke the URL that *was* current when the effect ran,
// not the one that's current after the next setPreview commits.
import { useCallback, useEffect, useRef, useState } from "react";

import { fetchArtifactFile } from "@/api/runs";

// Content types we render inline in the preview modal. Anything else
// gets the "use Download" fallback — covers binaries the in-sandbox
// recipe might emit (zips, tarballs, sqlite dbs, …).
const TEXT_MIME_PREFIXES = ["text/", "application/json", "application/yaml", "application/xml"];

export interface PreviewState {
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

export interface UsePreviewResult {
  preview: PreviewState | null;
  // openPreview fetches an artifact file for preview. `runIdOverride` targets
  // a run other than the hook's default — used by the pipeline sidebar to
  // preview a sub-bot's output (each produced item names its owning run).
  openPreview: (
    target: { path: string; size: number },
    runIdOverride?: string,
  ) => void;
  closePreview: () => void;
}

export function usePreview(runId: string | null): UsePreviewResult {
  const [preview, setPreview] = useState<PreviewState | null>(null);
  const previewGenRef = useRef(0);

  // A pipeline review turn can disappear while its media fetch is in flight
  // (the operator advances to the next queued review). Invalidate that fetch
  // on unmount so it cannot create an orphaned ObjectURL afterwards.
  useEffect(
    () => () => {
      ++previewGenRef.current;
    },
    [],
  );

  // Revoke the blob URL we created for the *previous* preview when
  // the URL value changes. Capture the URL in the closure so the
  // cleanup function frees the right one — a prior implementation
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
    // Invalidate an in-flight fetch as well as clearing visible state. Without
    // this, closing a loading review attachment lets its late response set a
    // fresh preview (and object URL) after the dialog has already disappeared.
    ++previewGenRef.current;
    setPreview(null);
  }, []);

  const openPreview = useCallback(
    (target: { path: string; size: number }, runIdOverride?: string) => {
      const effectiveRunId = runIdOverride ?? runId;
      if (!effectiveRunId) return;
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
      fetchArtifactFile(effectiveRunId, target.path)
        .then(async ({ blob, contentType }) => {
          if (myGen !== previewGenRef.current) return;
          const isText = TEXT_MIME_PREFIXES.some((p) => contentType.startsWith(p));
          if (isText) {
            const textBody = await blob.text();
            // blob.text() is a second asynchronous boundary. The operator may
            // have closed this preview or opened another file while decoding;
            // do not let that stale text resurrect/replace the current dialog.
            if (myGen !== previewGenRef.current) return;
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

  return { preview, openPreview, closePreview };
}
