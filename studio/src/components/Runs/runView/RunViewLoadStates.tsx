// Extracted from RunView.tsx to keep that file focused.
// The two presentational states the run console shows before its live
// data is ready: a load-error panel (snapshot fetch failed past the
// retry budget) and the loading skeleton.

import { useLocation } from "wouter";

import { Button, Skeleton } from "@/components/ui";
import { useServerInfoStore } from "@/store/serverInfo";

// RunViewLoadError renders when the initial REST snapshot fetch fails
// past the retry budget. Replaces the indefinite skeleton so the user
// sees a clear "not found" / error message + an actionable Back link.
// Common cause: clicking a run whose store the current daemon can't
// reach (e.g. a `~/.iterion/runs/...` global-slot run from a per-
// project desktop daemon — Open → 404).
//
// `onRetry` re-runs the in-place fetch loop (clears the failure flag and
// kicks the same routine the initial mount used). Used instead of a full
// `window.location.reload()` so the surrounding tabs, scroll position,
// and per-run UI state aren't destroyed when the operator retries.
export function RunViewLoadError({
  runId,
  status,
  message,
  onRetry,
}: {
  runId: string;
  status: number;
  message: string;
  onRetry: () => void;
}) {
  const [, setLocation] = useLocation();
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const isNotFound = status === 404;
  return (
    <div className="h-full w-full flex flex-col items-center justify-center gap-4 p-8">
      <div className="max-w-md text-center space-y-3 mt-4">
        <h2 className="text-base font-semibold text-fg-default">
          {isNotFound ? "Run not found" : "Run failed to load"}
        </h2>
        <p className="text-xs text-fg-muted font-mono break-all">{runId}</p>
        {isNotFound ? (
          cloud ? (
            <>
              <p className="text-xs text-fg-muted">
                You may not have access to this run, or it has been deleted.
                Try switching team / org via the sidebar, or pick a different
                run from the list.
              </p>
              <details className="text-caption text-fg-subtle">
                <summary className="cursor-pointer">Why might this happen?</summary>
                <p className="mt-1 text-left">
                  Runs are scoped to a (org, team) context. A shared link opened
                  under the wrong team returns 404 even when the run exists on
                  another team you belong to. The run may also have been deleted
                  by the operator who owned it.
                </p>
              </details>
            </>
          ) : (
            <>
              <p className="text-xs text-fg-muted">
                Open the project this run belongs to, or pick a different run from the
                list.
              </p>
              <details className="text-caption text-fg-subtle">
                <summary className="cursor-pointer">Why might this happen?</summary>
                <p className="mt-1 text-left">
                  The run may live in a different iterion store — for example, the
                  global <code>~/.iterion/runs/</code> slot served by another daemon,
                  or a per-project store this studio instance hasn&apos;t opened.
                </p>
              </details>
            </>
          )
        ) : (
          <p className="text-xs text-fg-muted">{message}</p>
        )}
        <div className="flex justify-center gap-2 pt-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => setLocation("/runs")}
          >
            Back to runs
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={onRetry}
          >
            Retry
          </Button>
        </div>
      </div>
    </div>
  );
}

export function RunViewSkeleton() {
  return (
    <div
      className="h-screen w-screen flex flex-col bg-surface-0"
      role="status"
      aria-live="polite"
      aria-busy="true"
    >
      <span className="sr-only">Loading run console…</span>
      <div className="border-b border-border-default px-4 py-2 flex items-center gap-3">
        <Skeleton className="h-6 w-16" />
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-5 w-20" />
        <div className="ml-auto">
          <Skeleton className="h-5 w-32" />
        </div>
      </div>
      <div className="flex-1 grid" style={{ gridTemplateColumns: "1fr 360px" }}>
        <div className="p-4">
          <Skeleton className="h-full w-full" />
        </div>
        <div className="p-4 border-l border-border-default space-y-2">
          <Skeleton className="h-5 w-32" />
          <Skeleton className="h-3 w-24" />
          <Skeleton className="h-32 w-full" />
        </div>
      </div>
      <div className="h-32 border-t border-border-default p-2">
        <Skeleton className="h-full w-full" />
      </div>
    </div>
  );
}
