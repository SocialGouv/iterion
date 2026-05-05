import { useState, type ReactNode } from "react";

import { CopyIcon } from "@radix-ui/react-icons";

import { StatusBadge, Tooltip } from "@/components/ui";
import { formatDurationBetween, formatRelative } from "@/lib/format";
import type { RunHeader } from "@/api/runs";

interface InfoPanelProps {
  run: RunHeader | null;
}

// InfoPanel renders a compact summary of the run's metadata: workflow
// + identity, status + timing, inputs, worktree refs, and merge state.
// The other tabs (Files, Commits) cover the actual artefacts; this one
// is for "where did this run come from and where are its commits?".
export default function InfoPanel({ run }: InfoPanelProps) {
  if (!run) {
    return (
      <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full items-center justify-center px-3 py-8 text-center text-xs text-fg-subtle">
        Loading…
      </div>
    );
  }

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full overflow-y-auto">
      <div className="px-3 py-2 space-y-3">
        <Section title="Run">
          <Row label="Status">
            <StatusBadge status={run.status} />
          </Row>
          <Row label="Name">
            <span className="truncate">{run.name || run.workflow_name}</span>
          </Row>
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
        </Section>

        <Section title="Timing">
          <Row label="Started">
            <span title={run.created_at}>{formatRelative(run.created_at)}</span>
          </Row>
          {run.finished_at && (
            <Row label="Finished">
              <span title={run.finished_at}>
                {formatRelative(run.finished_at)}
              </span>
            </Row>
          )}
          <Row label="Duration">
            <span>
              {formatDurationBetween(run.created_at, run.finished_at) ?? "—"}
            </span>
          </Row>
        </Section>

        {run.inputs && Object.keys(run.inputs).length > 0 && (
          <Section title="Inputs">
            {Object.entries(run.inputs).map(([k, v]) => (
              <Row key={k} label={k}>
                <Mono title={String(v)}>{truncate(String(v), 80)}</Mono>
              </Row>
            ))}
          </Section>
        )}

        <Section title="Worktree">
          <Row label="Mode">
            <span>{run.worktree ? "auto" : "inherited cwd"}</span>
          </Row>
          {run.work_dir && (
            <Row label="Path">
              <Mono copyable title={run.work_dir}>
                {basename(run.work_dir)}
              </Mono>
            </Row>
          )}
          {run.final_commit && (
            <Row label="Final commit">
              <Mono copyable title={run.final_commit}>
                {run.final_commit.slice(0, 7)}
              </Mono>
            </Row>
          )}
          {run.final_branch && (
            <Row label="Storage branch">
              <Mono copyable>{run.final_branch}</Mono>
            </Row>
          )}
        </Section>

        {run.worktree && (
          <Section title="Merge">
            <Row label="Strategy">
              <span>{run.merge_strategy || "squash"}</span>
            </Row>
            <Row label="Auto-merge">
              <span>{run.auto_merge ? "on" : "off"}</span>
            </Row>
            <Row label="Status">
              <span>{run.merge_status || "—"}</span>
            </Row>
            {run.merged_into && (
              <Row label="Merged into">
                <Mono copyable>{run.merged_into}</Mono>
              </Row>
            )}
            {run.merged_commit && (
              <Row label="Merged SHA">
                <Mono copyable title={run.merged_commit}>
                  {run.merged_commit.slice(0, 7)}
                </Mono>
              </Row>
            )}
          </Section>
        )}

        {run.error && (
          <Section title="Error">
            <div className="text-[11px] text-danger-fg bg-danger-soft px-2 py-1.5 rounded whitespace-pre-wrap">
              {run.error}
            </div>
          </Section>
        )}
      </div>
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  return (
    <section>
      <h3 className="text-[10px] font-semibold uppercase tracking-wide text-fg-muted mb-1">
        {title}
      </h3>
      <div className="space-y-1">{children}</div>
    </section>
  );
}

function Row({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="grid grid-cols-[80px_1fr] gap-2 text-[11px]">
      <span className="text-fg-subtle truncate">{label}</span>
      <div className="min-w-0 truncate text-fg-default">{children}</div>
    </div>
  );
}

interface MonoProps {
  children: string;
  copyable?: boolean;
  title?: string;
}

function Mono({ children, copyable, title }: MonoProps) {
  const [copied, setCopied] = useState(false);
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(children);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable in insecure contexts — silent
    }
  };
  if (!copyable) {
    return (
      <code className="font-mono text-[10px] text-fg-default" title={title}>
        {children}
      </code>
    );
  }
  return (
    <Tooltip content={copied ? "Copied" : title ?? "Click to copy"}>
      <button
        type="button"
        onClick={() => void onCopy()}
        className="inline-flex items-center gap-1 font-mono text-[10px] text-fg-default hover:text-info"
      >
        <span className="truncate">{children}</span>
        <CopyIcon className="h-3 w-3 shrink-0 text-fg-subtle" />
      </button>
    </Tooltip>
  );
}

function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? path : path.slice(i + 1);
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : s.slice(0, n - 1) + "…";
}
