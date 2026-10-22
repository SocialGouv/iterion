// The run's artifact-files listing and the two behaviours every consumer
// of it shares: a live refresh driven by the run's own events, and the
// download that routes through the desktop save path and the downloads
// history. The files panel and the header's deliverables row both read
// the same query key, so whichever is mounted keeps the other fresh.
import { useCallback, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { downloadArtifactFile, listArtifactFiles } from "@/api/runs";
import { useDownloadsStore } from "@/store/downloads";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

// Events that suggest a tool just dropped a new file. Refresh on
// node_finished (write_audit_md / emit_sbom complete here) and on the
// terminal triplet (run_finished + run_failed + run_cancelled) so the
// final SBOM shows up without the operator hitting refresh.
const REFRESH_EVENTS = new Set(["node_finished", "run_finished", "run_failed", "run_cancelled"]);
const DEBOUNCE_MS = 300;

export const artifactFilesQueryKey = (runId: string) => ["run-artifact-files", runId] as const;

export function useArtifactFilesQuery(runId: string | null) {
  return useQuery({
    queryKey: artifactFilesQueryKey(runId ?? ""),
    queryFn: () => listArtifactFiles(runId ?? ""),
    enabled: !!runId,
  });
}

// useArtifactFilesRefresh invalidates the listing when the run's events
// say a file may have landed. Tracking the last seen seq avoids
// re-triggering on the same event after a re-render; the pending timer
// is cleared on unmount so a consumer torn down within the debounce
// window does not invalidate after it is gone.
export function useArtifactFilesRefresh(runId: string | null): void {
  const queryClient = useQueryClient();
  const events = useRunStore((s) => s.events);
  const lastSeenSeqRef = useRef<number>(-1);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (!runId || events.length === 0) return;
    let touched = false;
    for (const ev of events) {
      if (ev.seq <= lastSeenSeqRef.current) continue;
      lastSeenSeqRef.current = ev.seq;
      if (REFRESH_EVENTS.has(ev.type)) touched = true;
    }
    if (!touched) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: artifactFilesQueryKey(runId) });
    }, DEBOUNCE_MS);
    return () => {
      if (debounceRef.current) {
        clearTimeout(debounceRef.current);
        debounceRef.current = null;
      }
    };
  }, [events, runId, queryClient]);
}

export interface ArtifactFileTarget {
  path: string;
  size: number;
}

// useArtifactDownload returns the one download gesture: the auth-aware
// fetch with `download=1`, the desktop save binding when embedded, the
// downloads-history record, and a toast whose "Open" action hands the
// file back to the caller's preview.
export function useArtifactDownload(
  runId: string | null,
  onOpen?: (target: ArtifactFileTarget) => void,
): (target: ArtifactFileTarget) => void {
  const recordDownload = useDownloadsStore((s) => s.recordDownload);
  const addToast = useUIStore((s) => s.addToast);
  return useCallback(
    (target: ArtifactFileTarget) => {
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
          addToast(
            `Downloaded ${basename}`,
            "success",
            onOpen ? { action: { label: "Open", onClick: () => onOpen(target) } } : undefined,
          );
        })
        .catch((err: unknown) => {
          addToast(
            `Download failed: ${err instanceof Error ? err.message : "unknown error"}`,
            "error",
          );
        });
    },
    [runId, recordDownload, addToast, onOpen],
  );
}
