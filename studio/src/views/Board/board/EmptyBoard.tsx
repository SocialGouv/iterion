import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useServerInfoStore } from "@/store/serverInfo";

// EmptyBoard renders the "tracker not initialised" guide. The "board
// exists but has no issues" case is handled by EmptyBoardBanner so the
// column headers stay visible. The local recipe (start the studio from
// the workspace) is meaningless against a SaaS backend, so cloud mode
// gets the real failure + a retry instead.
export function EmptyBoard({
  kind,
  error,
  onRetry,
}: {
  kind: "missing";
  // Fetch error from useBoardData, when the board is null because the
  // request failed rather than because the tracker is absent.
  error?: string | null;
  onRetry?: () => void;
}) {
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  if (kind !== "missing") return null;

  if (cloud) {
    return (
      <div className="p-8 max-w-lg mx-auto text-fg-default space-y-4">
        <div className="text-lg font-semibold">Board unavailable</div>
        {error ? (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        ) : (
          <p className="text-sm text-fg-muted">
            The server didn&apos;t return a board for this team. This is
            usually transient — retry, and if it persists contact your
            administrator.
          </p>
        )}
        {onRetry && (
          <Button variant="secondary" size="sm" onClick={onRetry}>
            Retry
          </Button>
        )}
      </div>
    );
  }

  return (
    <div className="p-8 max-w-lg mx-auto text-fg-default space-y-4">
      <div className="text-lg font-semibold">Native tracker not initialised</div>
      <p className="text-sm text-fg-muted">
        The board view persists issues under the project's{" "}
        <code className="text-xs bg-surface-2 px-1 rounded">.iterion/dispatcher/native/</code>{" "}
        directory. iterion creates one automatically on first launch.
      </p>
      <div className="text-sm">
        <p className="mb-1 text-fg-default">Start it from the workspace:</p>
        <pre className="bg-surface-2 rounded p-2 text-xs font-mono overflow-x-auto">
          iterion studio --dir &lt;your-project&gt;
        </pre>
      </div>
    </div>
  );
}
