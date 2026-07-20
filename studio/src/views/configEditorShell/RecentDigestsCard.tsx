import { useQuery } from "@tanstack/react-query";

import { listEditorShareRuns, type EditorShare } from "@/api/configEditor";
import { Badge } from "@/components/ui/Badge";
import { Card } from "@/components/ui";
import { STATUS_VARIANT, labelForStatus } from "@/components/Runs/runStatusMeta";
import type { RunStatus } from "@/api/runs";
import { formatRelative } from "@/lib/format";

// ---------------------------------------------------------------------------
// RecentDigestsCard — a read-only glance at the recent runs of this share's
// (bot, category), so the editor can see the effect of their edits: did the
// last digest run, when, did it succeed. Status + timestamps only (the server
// never returns run ids/inputs/errors to the config_editor role). Self-hides
// when the feature is unavailable or there are no runs yet — it never breaks
// the content editor.
// ---------------------------------------------------------------------------

export function RecentDigestsCard({
  teamID,
  share,
}: {
  teamID: string;
  share: EditorShare;
}) {
  const runsQuery = useQuery({
    queryKey: ["config-editor-runs", teamID, share.id],
    queryFn: () => listEditorShareRuns(teamID, share.id),
    enabled: !!teamID && !!share.id,
    // The digest cadence is slow (daily/weekly); a short poll is wasteful.
    staleTime: 60_000,
  });

  const runs = runsQuery.data;
  // Self-hide while loading, on error (incl. FeatureUnavailable), or when the
  // veille has never run — there is nothing useful to show.
  if (!runs || runs.length === 0) return null;

  return (
    <Card>
      <div className="flex flex-col gap-2">
        <div className="flex items-baseline justify-between">
          <h3 className="text-sm font-medium text-fg-default">Recent digests</h3>
          <span className="text-caption text-fg-subtle">
            {share.category ? share.category : "this veille"}
          </span>
        </div>
        <ul className="flex flex-col gap-1">
          {runs.map((run, i) => {
            const when = run.finished_at || run.created_at;
            return (
              <li
                key={`${run.created_at}-${i}`}
                className="flex items-center justify-between gap-2 rounded-md px-1 py-0.5 text-sm"
              >
                <Badge variant={STATUS_VARIANT[run.status as RunStatus] ?? "neutral"}>
                  {labelForStatus(run.status as RunStatus)}
                </Badge>
                <span className="text-caption text-fg-muted" title={when}>
                  {formatRelative(when)}
                </span>
              </li>
            );
          })}
        </ul>
        <p className="text-caption text-fg-subtle">
          A digest runs on the cadence above and posts to its channel. Your edits take
          effect on the next run.
        </p>
      </div>
    </Card>
  );
}
