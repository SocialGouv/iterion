// Extracted from BotHome/index.tsx to keep that file focused.
// Recent runs card — the bot's last runs with status badges, linking
// into the run console.

import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import { listRuns, type RunSummary } from "@/api/runs";
import { STATUS_VARIANT, labelForStatus } from "@/components/Runs/runStatusMeta";
import { Badge, Card, InlineBanner, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";

import SectionTitle from "./SectionTitle";

export default function RecentRunsCard({ botName }: { botName: string }) {
  const runsQuery = useQuery<RunSummary[]>({
    queryKey: ["bot-runs", botName],
    queryFn: () => listRuns({ bot: botName, limit: 10 }),
  });
  const runs = runsQuery.data ?? null;
  const err = runsQuery.error ? errorMessage(runsQuery.error) : null;

  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle flush>Recent runs</SectionTitle>
        <Link href="/runs" className="text-caption text-accent-text hover:underline">
          All runs →
        </Link>
      </div>
      {err && (
        <InlineBanner tone="danger" title="Couldn't load runs">
          {err}
        </InlineBanner>
      )}
      {runs === null && !err ? (
        <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
          <Spinner /> Loading runs…
        </div>
      ) : (runs ?? []).length === 0 && !err ? (
        <p className="py-1 text-xs text-fg-subtle">No runs yet for this bot.</p>
      ) : (
        <ul className="mt-1 divide-y divide-border-default">
          {(runs ?? []).map((r) => (
            <li key={r.id}>
              <Link
                href={`/runs/${encodeURIComponent(r.id)}`}
                className="flex items-center gap-2 rounded px-1 py-1.5 hover:bg-surface-2"
              >
                <Badge variant={STATUS_VARIANT[r.status] ?? "neutral"}>
                  {labelForStatus(r.status)}
                </Badge>
                <span className="min-w-0 flex-1 truncate text-xs text-fg-default">
                  {r.name?.trim() || r.workflow_name}
                </span>
                <span className="shrink-0 font-mono text-caption text-fg-subtle">
                  {r.id.slice(0, 8)}
                </span>
                <span className="shrink-0 text-caption text-fg-subtle">
                  {formatRelative(r.created_at)}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
