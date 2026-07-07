import { basename, formatRelative } from "@/lib/format";
import type { RunHeader } from "@/api/runs";

import { Mono, Row } from "../InfoPrimitives";
import { OverviewSection } from "./OverviewSection";

interface AdvancedSectionProps {
  run: RunHeader;
}

// AdvancedSection is the collapsed-by-default detail sheet that replaces
// the former Info tab: run identity, the .bot source, the exec directory,
// timing, and the full error text. Everything already surfaced above (the
// hero's status, the outcome's commit/branch) is NOT repeated here.
export function AdvancedSection({ run }: AdvancedSectionProps) {
  return (
    <OverviewSection title="Advanced details" defaultOpen={false}>
      <Row label="ID">
        <Mono copyable>{run.id}</Mono>
      </Row>
      <Row label="Workflow">
        <span className="truncate">{run.workflow_name}</span>
      </Row>
      {run.file_path && (
        <Row label="Source">
          <Mono copyable title={run.file_path}>
            {basename(run.file_path)}
          </Mono>
        </Row>
      )}
      {run.work_dir && (
        <Row label="Work dir">
          <Mono copyable title={run.work_dir}>
            {basename(run.work_dir)}
          </Mono>
        </Row>
      )}
      <Row label="Started">
        <span title={run.created_at}>{formatRelative(run.created_at)}</span>
      </Row>
      {run.finished_at && (
        <Row label="Finished">
          <span title={run.finished_at}>{formatRelative(run.finished_at)}</span>
        </Row>
      )}
      {run.error && (
        <div className="mt-1.5 text-micro text-danger-fg bg-danger-soft px-2 py-1.5 rounded whitespace-pre-wrap">
          {run.error}
        </div>
      )}
    </OverviewSection>
  );
}
