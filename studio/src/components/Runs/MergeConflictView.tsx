import { useCallback, useEffect, useState } from "react";

import { Button, EmptyState, Spinner, Textarea } from "@/components/ui";
import {
  abortMergeConflict,
  finalizeMergeConflict,
  getMergeConflicts,
  resolveMergeConflict,
  resolveMergeConflictWithAgent,
  type MergeConflictFile,
  type MergeConflictHunk,
  type MergeConflictsResponse,
} from "@/api/runs";

interface MergeConflictViewProps {
  runId: string;
  defaultMessage?: string;
  onMergeComplete?: () => void;
}

// MergeConflictView is the per-file conflict resolver mounted inside
// the CommitsPanel footer when `merge_status === "conflicted"`. It
// fetches the conflict snapshot, lets the operator resolve each file
// via per-hunk quick actions or a free-form textarea, then finalizes
// the squash commit.
//
// The local model holds one "editable content" string per file —
// initialized from the wire content (markers included), mutated by
// the per-hunk action buttons (Take ours / Take theirs / Take both),
// and posted to the server as-is when the operator clicks "Resolve".
// Once every file has been staged (server returns files: []), the
// "Finish merge" button enables.
export default function MergeConflictView({
  runId,
  defaultMessage,
  onMergeComplete,
}: MergeConflictViewProps) {
  const [snapshot, setSnapshot] = useState<MergeConflictsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Per-file local content the operator is editing. Keys are file
  // paths; values are the working text the next "Resolve" submission
  // will send. Reset on refresh to whatever the server returns.
  const [working, setWorking] = useState<Record<string, string>>({});
  // Per-file collapsed flag — by default all expand so the operator
  // sees the full scope.
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const [busyFile, setBusyFile] = useState<string | null>(null);
  const [busyGlobal, setBusyGlobal] = useState<null | "agent" | "finalize" | "abort">(
    null,
  );
  const [overrideMessage, setOverrideMessage] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await getMergeConflicts(runId);
      setSnapshot(next);
      setWorking((prev) => {
        const merged: Record<string, string> = {};
        for (const f of next.files) {
          // Preserve in-flight edits when the server still lists the
          // file; otherwise reset to fresh content. The user might
          // have typed 50 lines into a textarea — a background
          // refresh shouldn't blow that away.
          merged[f.path] = prev[f.path] ?? f.content;
        }
        return merged;
      });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const remaining = snapshot?.files ?? [];
  const allResolved = !loading && remaining.length === 0;
  const finalizeMessage =
    overrideMessage ?? snapshot?.pending_message ?? defaultMessage ?? "";

  const onResolveFile = async (path: string) => {
    const content = working[path];
    if (content === undefined) return;
    setBusyFile(path);
    setError(null);
    try {
      const next = await resolveMergeConflict(runId, { path, content });
      setSnapshot(next);
      setWorking((prev) => {
        const updated = { ...prev };
        delete updated[path];
        for (const f of next.files) {
          updated[f.path] = updated[f.path] ?? f.content;
        }
        return updated;
      });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusyFile(null);
    }
  };

  const onResolveWithAgent = async () => {
    setBusyGlobal("agent");
    setError(null);
    try {
      const next = await resolveMergeConflictWithAgent(runId);
      setSnapshot(next);
      // Agent overwrites content; fold the new server-side text into
      // our editable buffer so the operator can review + tweak.
      setWorking(() => {
        const fresh: Record<string, string> = {};
        for (const f of next.files) fresh[f.path] = f.content;
        return fresh;
      });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusyGlobal(null);
    }
  };

  const onFinalize = async () => {
    setBusyGlobal("finalize");
    setError(null);
    try {
      await finalizeMergeConflict(runId, { message: finalizeMessage || undefined });
      onMergeComplete?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusyGlobal(null);
    }
  };

  const onAbort = async () => {
    if (
      typeof window !== "undefined" &&
      !window.confirm(
        "Abort the merge? The worktree will reset to the target branch; any in-progress resolutions will be lost.",
      )
    ) {
      return;
    }
    setBusyGlobal("abort");
    setError(null);
    try {
      await abortMergeConflict(runId);
      onMergeComplete?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusyGlobal(null);
    }
  };

  if (loading && !snapshot) {
    return (
      <div className="shrink-0 border-t border-border-default px-3 py-3 text-[11px] text-fg-subtle bg-warning-soft">
        <Spinner /> Loading conflicts…
      </div>
    );
  }

  if (error && !snapshot) {
    return (
      <div className="shrink-0 border-t border-border-default px-3 py-2 text-[11px] text-danger-fg bg-danger-soft">
        {error}
        <button
          type="button"
          onClick={() => void refresh()}
          className="ml-2 underline"
        >
          Retry
        </button>
      </div>
    );
  }

  return (
    <div className="shrink-0 border-t border-border-default bg-warning-soft max-h-[70%] overflow-y-auto">
      <header className="sticky top-0 z-10 flex items-center gap-2 border-b border-border-default bg-warning-soft px-3 py-2">
        <span className="text-[11px] font-semibold text-warning-fg">
          Merge conflict
        </span>
        <span className="text-[10px] text-fg-subtle">
          {remaining.length === 0
            ? "All files resolved"
            : `${remaining.length} file${remaining.length === 1 ? "" : "s"} pending`}
        </span>
        <div className="ml-auto flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void onResolveWithAgent()}
            disabled={busyGlobal !== null || remaining.length === 0}
          >
            {busyGlobal === "agent" ? "Resolving…" : "Resolve all with agent"}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void onAbort()}
            disabled={busyGlobal !== null}
          >
            {busyGlobal === "abort" ? "Aborting…" : "Abort merge"}
          </Button>
        </div>
      </header>
      {error && (
        <div className="border-b border-border-default px-3 py-1 text-[10px] text-danger-fg bg-danger-soft">
          {error}
        </div>
      )}
      {remaining.length === 0 ? (
        <div className="px-3 py-3">
          <EmptyState message="Every file resolved. Finalize when ready." />
        </div>
      ) : (
        <ul className="divide-y divide-border-default">
          {remaining.map((file) => (
            <ConflictFileCard
              key={file.path}
              file={file}
              content={working[file.path] ?? file.content}
              onContentChange={(next) =>
                setWorking((prev) => ({ ...prev, [file.path]: next }))
              }
              collapsed={collapsed[file.path] ?? false}
              onToggle={() =>
                setCollapsed((prev) => ({
                  ...prev,
                  [file.path]: !(prev[file.path] ?? false),
                }))
              }
              busy={busyFile === file.path}
              onResolve={() => void onResolveFile(file.path)}
              onReset={() =>
                setWorking((prev) => ({ ...prev, [file.path]: file.content }))
              }
            />
          ))}
        </ul>
      )}
      <footer className="sticky bottom-0 border-t border-border-default bg-warning-soft px-3 py-2 space-y-2">
        <div>
          <label className="block text-[10px] uppercase tracking-wide text-fg-subtle mb-1">
            Squash commit message
          </label>
          <Textarea
            rows={3}
            value={finalizeMessage}
            onChange={(e) => setOverrideMessage(e.target.value)}
            disabled={busyGlobal !== null}
            className="text-[11px] font-mono"
          />
        </div>
        <Button
          variant="primary"
          size="sm"
          className="w-full"
          onClick={() => void onFinalize()}
          disabled={!allResolved || busyGlobal !== null || !finalizeMessage.trim()}
        >
          {busyGlobal === "finalize" ? "Finalizing…" : "Finish merge"}
        </Button>
        {!allResolved && (
          <div className="text-[10px] text-fg-subtle">
            Resolve every file before finishing. Each file's "Resolve" button
            stages the current content via <code>git add</code>.
          </div>
        )}
      </footer>
    </div>
  );
}

interface ConflictFileCardProps {
  file: MergeConflictFile;
  content: string;
  onContentChange: (next: string) => void;
  collapsed: boolean;
  onToggle: () => void;
  busy: boolean;
  onResolve: () => void;
  onReset: () => void;
}

// ConflictFileCard renders one conflicted file: header with action
// buttons, per-hunk quick-action strip, then the editable textarea
// showing the current working content (markers + operator edits).
function ConflictFileCard({
  file,
  content,
  onContentChange,
  collapsed,
  onToggle,
  busy,
  onResolve,
  onReset,
}: ConflictFileCardProps) {
  const dirty = content !== file.content;
  if (file.read_err) {
    return (
      <li className="px-3 py-2">
        <div className="font-mono text-[11px] text-danger-fg">{file.path}</div>
        <div className="text-[10px] text-danger-fg">
          Cannot read file: {file.read_err}
        </div>
      </li>
    );
  }
  return (
    <li className="px-3 py-2">
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={onToggle}
          className="font-mono text-[11px] text-fg-default hover:underline"
        >
          {collapsed ? "▸" : "▾"} {file.path}
        </button>
        <span className="text-[10px] text-fg-subtle">
          {file.hunks.length} hunk{file.hunks.length === 1 ? "" : "s"}
        </span>
        <div className="ml-auto flex items-center gap-1">
          {dirty && (
            <Button variant="ghost" size="sm" onClick={onReset} disabled={busy}>
              Reset
            </Button>
          )}
          <Button
            variant="primary"
            size="sm"
            onClick={onResolve}
            disabled={busy || hasConflictMarkers(content)}
          >
            {busy ? "Resolving…" : "Resolve"}
          </Button>
        </div>
      </div>
      {!collapsed && (
        <div className="mt-2 space-y-2">
          {file.hunks.map((hunk, idx) => (
            <HunkPanel
              key={idx}
              index={idx + 1}
              hunk={hunk}
              onPickOurs={() =>
                onContentChange(applyHunk(content, hunk, "ours"))
              }
              onPickTheirs={() =>
                onContentChange(applyHunk(content, hunk, "theirs"))
              }
              onPickBoth={() =>
                onContentChange(applyHunk(content, hunk, "both"))
              }
            />
          ))}
          <Textarea
            rows={Math.min(20, Math.max(6, content.split("\n").length))}
            value={content}
            onChange={(e) => onContentChange(e.target.value)}
            className="font-mono text-[11px]"
            disabled={busy}
          />
          {hasConflictMarkers(content) && (
            <div className="text-[10px] text-warning-fg">
              File still contains conflict markers. Use a hunk action or
              remove them manually before resolving.
            </div>
          )}
        </div>
      )}
    </li>
  );
}

interface HunkPanelProps {
  index: number;
  hunk: MergeConflictHunk;
  onPickOurs: () => void;
  onPickTheirs: () => void;
  onPickBoth: () => void;
}

// HunkPanel is the side-by-side ours/theirs preview with the quick
// action strip. Base lines (diff3 mode) are shown collapsed by
// default beneath the ours pane.
function HunkPanel({
  index,
  hunk,
  onPickOurs,
  onPickTheirs,
  onPickBoth,
}: HunkPanelProps) {
  return (
    <div className="rounded border border-border-default bg-surface-0">
      <div className="flex items-center gap-1 border-b border-border-default px-2 py-1">
        <span className="text-[10px] font-medium text-fg-subtle">
          Hunk {index} · lines {hunk.start_line}–{hunk.end_line}
        </span>
        <div className="ml-auto flex items-center gap-1">
          <Button variant="ghost" size="sm" onClick={onPickOurs}>
            Take {hunk.ours_label || "ours"}
          </Button>
          <Button variant="ghost" size="sm" onClick={onPickTheirs}>
            Take {hunk.theirs_label || "incoming"}
          </Button>
          <Button variant="ghost" size="sm" onClick={onPickBoth}>
            Take both
          </Button>
        </div>
      </div>
      <div className="grid grid-cols-2 gap-px bg-border-default text-[10px]">
        <HunkPane label={`Ours (${hunk.ours_label || "HEAD"})`} lines={hunk.ours_lines} />
        <HunkPane
          label={`Incoming (${hunk.theirs_label || "branch"})`}
          lines={hunk.theirs_lines}
        />
      </div>
      {hunk.base_lines && hunk.base_lines.length > 0 && (
        <div className="border-t border-border-default text-[10px]">
          <HunkPane label="Base" lines={hunk.base_lines} muted />
        </div>
      )}
    </div>
  );
}

function HunkPane({
  label,
  lines,
  muted,
}: {
  label: string;
  lines: string[];
  muted?: boolean;
}) {
  return (
    <div className={`p-1 ${muted ? "bg-surface-1" : "bg-surface-0"}`}>
      <div className="mb-0.5 text-[9px] uppercase tracking-wide text-fg-subtle">
        {label}
      </div>
      <pre className="m-0 whitespace-pre-wrap break-words font-mono text-[11px] text-fg-default">
        {lines.length === 0 ? <em className="text-fg-subtle">(empty)</em> : lines.join("\n")}
      </pre>
    </div>
  );
}

// hasConflictMarkers returns true when content still includes a
// `<<<<<<<` or `>>>>>>>` marker. Used to gate the "Resolve" button
// so the operator can't stage a file that obviously isn't done.
function hasConflictMarkers(content: string): boolean {
  return /^(<{7}|={7}|>{7}|\|{7})/m.test(content);
}

// applyHunk produces a new content string with the named hunk
// replaced by the chosen lines. The hunk lives between line numbers
// hunk.start_line and hunk.end_line (1-indexed, inclusive). "both"
// concatenates ours then theirs (the conventional default when the
// operator wants the union).
function applyHunk(
  content: string,
  hunk: MergeConflictHunk,
  pick: "ours" | "theirs" | "both",
): string {
  // Split on \n and preserve the trailing-newline distinction by
  // letting the join re-introduce it.
  const lines = content.split("\n");
  const replacement = replacementLines(hunk, pick);
  const before = lines.slice(0, hunk.start_line - 1);
  const after = lines.slice(hunk.end_line);
  return [...before, ...replacement, ...after].join("\n");
}

function replacementLines(
  hunk: MergeConflictHunk,
  pick: "ours" | "theirs" | "both",
): string[] {
  switch (pick) {
    case "ours":
      return [...hunk.ours_lines];
    case "theirs":
      return [...hunk.theirs_lines];
    case "both":
      return [...hunk.ours_lines, ...hunk.theirs_lines];
  }
}

// Re-export for tests.
export const __testables = {
  applyHunk,
  hasConflictMarkers,
  replacementLines,
};
