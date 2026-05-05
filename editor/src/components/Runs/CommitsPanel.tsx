import { useMemo, useState } from "react";

import { ReloadIcon } from "@radix-ui/react-icons";

import {
  Button,
  IconButton,
  Select,
  Textarea,
  Tooltip,
} from "@/components/ui";
import { useRunCommits } from "@/hooks/useRunCommits";
import {
  mergeRun,
  type MergeStrategy,
  type RunCommit,
  type RunHeader,
} from "@/api/runs";
import { formatRelative } from "@/lib/format";

interface CommitsPanelProps {
  runId: string;
  run: RunHeader | null;
  // onMergeComplete fires after a successful merge so the parent can
  // refetch the run snapshot — the RunHeader badges depend on it.
  onMergeComplete?: () => void;
}

// CommitsPanel is the GitHub-PR-style listing for the run: every
// workflow `git commit` shows up as a row, and once the run has
// finished a merge form lets the user pick squash vs merge and
// confirm. Mounted inside LeftPanel; visible when the Commits tab
// is selected.
export default function CommitsPanel({
  runId,
  run,
  onMergeComplete,
}: CommitsPanelProps) {
  const { data, loading, error, refresh } = useRunCommits(runId);

  const commitCount = data?.commits.length ?? 0;

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full">
      <header className="flex items-center gap-1 px-2 py-1 border-b border-border-default">
        {commitCount > 0 && (
          <span className="inline-flex items-center justify-center rounded-md bg-surface-2 px-1.5 text-[10px] font-medium text-fg-muted">
            {commitCount}
          </span>
        )}
        <div className="ml-auto">
          <IconButton
            label="Refresh"
            size="sm"
            variant="ghost"
            onClick={refresh}
            disabled={loading}
          >
            <ReloadIcon className={loading ? "animate-spin" : undefined} />
          </IconButton>
        </div>
      </header>
      <div className="flex-1 min-h-0 overflow-y-auto">
        {error ? (
          <EmptyState message={error} />
        ) : !data ? (
          loading ? (
            <EmptyState message="Loading…" />
          ) : (
            <EmptyState message="" />
          )
        ) : !data.available ? (
          <EmptyState message={reasonLabel(data.reason)} />
        ) : data.commits.length === 0 ? (
          <EmptyState message="No commits yet" />
        ) : (
          <ul className="py-1">
            {data.commits.map((c) => (
              <CommitRow key={c.sha} commit={c} />
            ))}
          </ul>
        )}
      </div>
      {run && (
        <MergeFooter
          runId={runId}
          run={run}
          commitCount={commitCount}
          onMergeComplete={onMergeComplete}
        />
      )}
    </div>
  );
}

function CommitRow({ commit }: { commit: RunCommit }) {
  const relative = useMemo(() => formatRelative(commit.date), [commit.date]);
  return (
    <li>
      <Tooltip content={`${commit.subject}\n\n${commit.author} · ${commit.date}`}>
        <div className="flex w-full items-start gap-2 px-2 py-1.5 hover:bg-surface-2">
          <code className="text-[10px] font-mono text-fg-subtle pt-0.5 shrink-0">
            {commit.short}
          </code>
          <div className="min-w-0 flex-1">
            <div className="truncate text-xs text-fg-default">
              {commit.subject}
            </div>
            <div className="truncate text-[10px] text-fg-subtle">
              {commit.author} · {relative}
            </div>
          </div>
        </div>
      </Tooltip>
    </li>
  );
}

interface MergeFooterProps {
  runId: string;
  run: RunHeader;
  commitCount: number;
  onMergeComplete?: () => void;
}

function MergeFooter({ runId, run, commitCount, onMergeComplete }: MergeFooterProps) {
  // Only finished runs with a storage branch can be merged. Skipped
  // (merge_into=none) is treated as terminal and shows a passive note.
  const finished = run.status === "finished";
  const hasBranch = Boolean(run.final_branch);
  const merged = run.merge_status === "merged";
  const failed = run.merge_status === "failed";
  const skipped = run.merge_status === "skipped";

  const initialStrategy: MergeStrategy =
    (run.merge_strategy as MergeStrategy) ?? "squash";

  const [strategy, setStrategy] = useState<MergeStrategy>(initialStrategy);
  const [message, setMessage] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  if (!hasBranch || !finished) {
    if (skipped) {
      return (
        <div className="border-t border-border-default px-3 py-2 text-[10px] text-fg-subtle">
          Merge skipped (no target branch — storage branch preserved).
        </div>
      );
    }
    return null;
  }

  if (merged) {
    const shortMerged = (run.merged_commit ?? "").slice(0, 7);
    return (
      <div className="border-t border-border-default px-3 py-2 bg-success-soft text-success-fg text-[11px]">
        <div className="font-medium">
          {run.merge_strategy === "squash"
            ? "Squashed and merged"
            : "Merged"}{" "}
          into {run.merged_into}
        </div>
        {shortMerged && (
          <div className="font-mono text-[10px] mt-0.5">{shortMerged}</div>
        )}
      </div>
    );
  }

  const onSubmit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      const res = await mergeRun(runId, {
        merge_strategy: strategy,
        merge_into: undefined, // backend defaults to current branch
        commit_message: strategy === "squash" ? message || undefined : undefined,
      });
      if (res.merge_status === "merged") {
        onMergeComplete?.();
      }
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };

  const buttonLabel =
    strategy === "squash" ? "Squash and merge" : "Merge commit";

  return (
    <div className="border-t border-border-default px-3 py-2 space-y-2 bg-surface-1">
      {failed && (
        <div className="text-[10px] text-danger-fg bg-danger-soft px-2 py-1 rounded">
          Previous merge failed — fix the underlying issue (clean working
          tree, target branch checked out) and retry.
        </div>
      )}
      <div className="flex items-center gap-2">
        <span className="text-[10px] text-fg-subtle uppercase tracking-wide">
          Merge {commitCount} commit{commitCount === 1 ? "" : "s"}
        </span>
      </div>
      <Select
        value={strategy}
        onChange={(e) => setStrategy(e.target.value as MergeStrategy)}
        disabled={submitting}
      >
        <option value="squash">Squash and merge</option>
        <option value="merge">Merge commit (preserve history)</option>
      </Select>
      {strategy === "squash" && (
        <Textarea
          rows={3}
          placeholder="Optional override for the squash commit message — leave empty to use an auto-generated summary listing each squashed commit."
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          disabled={submitting}
          className="text-[11px] font-mono"
        />
      )}
      {err && (
        <div className="text-[10px] text-danger-fg bg-danger-soft px-2 py-1 rounded">
          {err}
        </div>
      )}
      <Button
        variant="primary"
        size="sm"
        onClick={() => void onSubmit()}
        disabled={submitting}
        className="w-full"
      >
        {submitting ? "Merging…" : buttonLabel}
      </Button>
      <div className="text-[10px] text-fg-subtle">
        Target: currently-checked-out branch. The merge fails fast if the
        working tree is dirty or the storage branch is not fast-forwardable.
      </div>
    </div>
  );
}

function EmptyState({ message }: { message: string }) {
  return (
    <div className="flex h-full items-center justify-center px-3 py-8 text-center text-xs text-fg-subtle">
      {message}
    </div>
  );
}

function reasonLabel(reason: string | undefined): string {
  switch (reason) {
    case "no_workdir":
      return "No working directory recorded for this run";
    case "no_baseline":
      return "Run has no recorded base commit — cannot compute commit list";
    case "not_git_repo":
      return "Not a git repository";
    default:
      return reason ?? "Commits unavailable";
  }
}

