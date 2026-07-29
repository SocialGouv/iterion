import type { PipelineBoardCard } from "@/api/pipelineBoards";
import {
  STATUS_VARIANT,
  labelForStatus,
} from "@/components/Runs/runStatusMeta";
import { Badge, InlineBanner, LiveDot } from "@/components/ui";
import { useGlobalActiveRuns } from "@/hooks/useGlobalActiveRuns";
import { useProjectInfo } from "@/hooks/useProjectInfo";
import { formatRelative } from "@/lib/format";

import {
  crossStoreRunHref,
  externalActiveRuns,
} from "./externalActiveRuns";

interface Props {
  cards: readonly PipelineBoardCard[];
}

export default function ExternalActiveRunsNotice({ cards }: Props) {
  const { runs, error } = useGlobalActiveRuns();
  const { dir: projectDir } = useProjectInfo();
  const external = externalActiveRuns(runs, cards, projectDir);

  if (error && typeof console !== "undefined") {
    console.warn("PipelineBoard: listGlobalActiveRuns failed:", error);
  }
  if (external.length === 0) return null;

  return (
    <InlineBanner
      tone="info"
      layout="inline"
      title={`${external.length} project ${
        external.length === 1 ? "run is" : "runs are"
      } stored outside this board`}
    >
      <p className="mt-0.5 text-fg-muted">
        {external.length === 1 ? "This run was" : "These runs were"} launched
        or resumed from a CLI store different from Studio&apos;s store, often
        because{" "}
        <code className="font-mono">--store-dir</code> was omitted or differed.
        Cards below still show their own persisted attempts, so an older card
        may remain Paused.
      </p>
      <ul className="mt-2 space-y-1" aria-label="Project runs in other stores">
        {external.map((run) => (
          <li key={`${run.store_path}:${run.id}`}>
            <a
              href={crossStoreRunHref(run)}
              className="flex min-w-0 items-center gap-2 rounded border border-info/30 bg-surface-1 px-2 py-1.5 text-fg-default hover:border-info/60 hover:bg-surface-2"
            >
              <LiveDot tone="info" size="sm" className="shrink-0" />
              <span className="min-w-0 flex-1">
                <span className="block truncate font-medium">
                  {run.bundle_display_name ||
                    run.bundle_name ||
                    run.workflow_name}
                </span>
                <span className="block truncate text-caption text-fg-muted">
                  {run.workflow_name}
                  {run.input_path ? ` · ${run.input_path}` : ""}
                </span>
                <span
                  className="block truncate font-mono text-caption text-fg-subtle"
                  title={run.store_path}
                >
                  {run.id.slice(0, 12)} · {run.store_path}
                </span>
              </span>
              <Badge
                variant={STATUS_VARIANT[run.status] ?? "info"}
                className="shrink-0"
              >
                {labelForStatus(run.status)}
              </Badge>
              <span
                className="shrink-0 text-caption text-fg-subtle"
                title={run.updated_at}
              >
                {formatRelative(run.updated_at)}
              </span>
            </a>
          </li>
        ))}
      </ul>
    </InlineBanner>
  );
}
