import { Button } from "@/components/ui";
import { useRunFiles } from "@/hooks/useRunFiles";
import { useRunCommits } from "@/hooks/useRunCommits";
import type { RunHeader } from "@/api/runs";

import { Mono, Row, Section } from "../InfoPrimitives";

interface OutcomeSectionProps {
  runId: string;
  run: RunHeader;
  onSwitchTab: (tab: "files" | "commits") => void;
}

// OutcomeSection is where the run's work LANDED: the final commit, the
// storage branch (or its recovery hint), the merge disposition, and quick
// jumps into the Files / Commits tabs with live change counts. Hidden
// until there's an outcome to describe — a running or just-started run
// has none yet. Absorbs the worktree/merge data that used to live on the
// Info tab, with an operator-actionable twist (the cross-tab buttons).
export function OutcomeSection({ runId, run, onSwitchTab }: OutcomeSectionProps) {
  // Match FilesPanel's default mode so react-query dedupes onto one cache
  // entry (no extra fetch) and the "N files changed" count agrees with the
  // Files tab (mode "" = uncommitted only, a subset of combined).
  const files = useRunFiles(runId, "combined");
  const commits = useRunCommits(runId);
  const fileCount = files.data?.files?.length ?? 0;
  const commitCount = commits.data?.commits?.length ?? 0;

  const hasOutcome =
    !!run.final_commit ||
    !!run.merged_into ||
    fileCount > 0 ||
    commitCount > 0;
  if (!hasOutcome) return null;

  return (
    <Section title="Outcome">
      {run.final_commit && (
        <Row label="Commit">
          <Mono copyable title={run.final_commit}>
            {run.final_commit.slice(0, 7)}
          </Mono>
        </Row>
      )}

      {run.final_branch && !run.final_branch_error && (
        <Row label="Branch">
          <Mono copyable>{run.final_branch}</Mono>
        </Row>
      )}
      {run.final_branch_error && (
        <Row label="Branch">
          <details className="text-danger-fg" title={run.final_branch_error}>
            <summary className="cursor-pointer">creation failed</summary>
            <div className="mt-1">
              Recover with{" "}
              <Mono>{`git branch <name> ${run.final_commit?.slice(0, 7) ?? ""}`}</Mono>
            </div>
          </details>
        </Row>
      )}

      {run.worktree && (
        <Row label="Merge">
          <span>{mergeStatusLabel(run.merge_status)}</span>
        </Row>
      )}
      {run.merged_into && (
        <Row label="Merged into">
          <Mono copyable>{run.merged_into}</Mono>
        </Row>
      )}

      {(fileCount > 0 || commitCount > 0) && (
        <div className="flex flex-wrap gap-2 pt-1.5">
          {fileCount > 0 && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => onSwitchTab("files")}
            >
              {fileCount} file{fileCount === 1 ? "" : "s"} changed →
            </Button>
          )}
          {commitCount > 0 && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => onSwitchTab("commits")}
            >
              {commitCount} commit{commitCount === 1 ? "" : "s"} →
            </Button>
          )}
        </div>
      )}
    </Section>
  );
}

// mergeStatusLabel humanises the raw RunHeader.merge_status string — the
// engine uses short identifiers; operators read full English here.
// Inlined from the former InfoPanel so its Merge detail survives the fold.
function mergeStatusLabel(s: RunHeader["merge_status"] | undefined): string {
  switch (s) {
    case "merged":
      return "Merged";
    case "pending":
      return "Awaiting merge";
    case "failed":
      return "Merge failed";
    case "skipped":
      return "Skipped";
    case "conflicted":
      return "Merge conflict — resolve in Commits tab";
    case "merging":
      return "Merge in progress…";
    default:
      return s || "—";
  }
}
