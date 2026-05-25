// Findings view — the operator-facing inbox.
//
// Bots emit findings to ${PROJECT_MEMORY_DIR}/findings/ when they
// notice something worth tracking but that doesn't fit the current
// roadmap (a bug spotted during a feature_dev run, a security smell
// noticed during sec-audit, a doc drift caught by doc-align). The
// findings inbox sat at the filesystem layer until now — operators
// `ls`d the directory and read raw markdown. This view exposes the
// inbox to the studio: list, filter, preview, and archive when the
// finding has been actioned (or was a false positive).
//
// Archiving = DELETE of the .md file. The runtime's whats-next bot
// also has a step that auto-archives findings whose actionable next
// step landed in a created issue (see emit_action_system "Findings
// inbox cleanup"), so the operator's interaction here covers the
// false-positive / superseded path that automation can't infer.

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "wouter";
import { ArrowLeftIcon } from "@radix-ui/react-icons";

import { deleteFinding, listFindings, type Finding } from "@/api/findings";
import { Button } from "@/components/ui/Button";
import { ErrorBoundary } from "@/components/shared/ErrorBoundary";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import { formatRelative } from "@/lib/format";

export default function FindingsView() {
  return (
    <ErrorBoundary area="Findings view">
      <FindingsViewInner />
    </ErrorBoundary>
  );
}

function FindingsViewInner() {
  const [findings, setFindings] = useState<Finding[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [filterText, setFilterText] = useState("");
  const [filterKind, setFilterKind] = useState("");
  const [filterSource, setFilterSource] = useState("");
  const [busyId, setBusyId] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const next = await listFindings();
      setFindings(next);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const { kinds, sources } = useMemo(() => {
    const k = new Set<string>();
    const s = new Set<string>();
    for (const f of findings ?? []) {
      if (f.kind) k.add(f.kind);
      if (f.source_bot) s.add(f.source_bot);
    }
    return { kinds: [...k].sort(), sources: [...s].sort() };
  }, [findings]);

  const filtered = useMemo(() => {
    if (!findings) return null;
    const q = filterText.trim().toLowerCase();
    return findings.filter((f) => {
      if (filterKind && f.kind !== filterKind) return false;
      if (filterSource && f.source_bot !== filterSource) return false;
      if (q) {
        const hay = [
          f.title ?? "",
          f.description ?? "",
          f.body ?? "",
          f.filename,
          ...(f.tags ?? []),
        ]
          .join("\n")
          .toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
  }, [findings, filterText, filterKind, filterSource]);

  const onArchive = useCallback(
    async (f: Finding) => {
      const ok = confirm(
        `Archive (delete) ${f.filename}? The runtime can re-emit it if the underlying issue resurfaces.`,
      );
      if (!ok) return;
      setBusyId(f.filename);
      try {
        await deleteFinding(f.filename);
        await refresh();
        if (expandedId === f.filename) setExpandedId(null);
      } catch (e) {
        setError((e as Error).message);
      } finally {
        setBusyId(null);
      }
    },
    [refresh, expandedId],
  );

  const total = findings?.length ?? 0;

  return (
    <div className="h-full overflow-auto p-4 space-y-3 text-[13px]">
      <header className="flex items-baseline gap-3">
        <Link
          href="/runs"
          className="text-fg-muted hover:text-fg-default text-[11px] inline-flex items-center gap-1 shrink-0"
        >
          <ArrowLeftIcon className="w-3 h-3" />
          Runs
        </Link>
        <h1 className="text-lg font-semibold text-fg-default">
          Findings inbox
        </h1>
        <span className="text-fg-muted text-[11px]">
          {total} open
        </span>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void refresh()}
          className="ml-auto"
        >
          Refresh
        </Button>
      </header>

      <p className="text-fg-muted text-[11px] max-w-3xl">
        Bot runs write notes here when they spot something worth
        tracking but not worth dispatching alone (a bug surfaced during
        a feature run, a doc drift, a security smell). The next
        whats-next session reads this inbox and may promote items to
        the board. Archive a finding when it's been acted on or turned
        out to be a false positive.
      </p>

      <div className="flex flex-wrap items-center gap-2 text-[11px]">
        <input
          type="text"
          placeholder="Search title / body / tags…"
          value={filterText}
          onChange={(e) => setFilterText(e.target.value)}
          className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default w-56"
        />
        {kinds.length > 0 && (
          <select
            value={filterKind}
            onChange={(e) => setFilterKind(e.target.value)}
            className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default"
          >
            <option value="">all kinds</option>
            {kinds.map((k) => (
              <option key={k} value={k}>
                kind:{k}
              </option>
            ))}
          </select>
        )}
        {sources.length > 0 && (
          <select
            value={filterSource}
            onChange={(e) => setFilterSource(e.target.value)}
            className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default"
          >
            <option value="">all bots</option>
            {sources.map((s) => (
              <option key={s} value={s}>
                source:{s}
              </option>
            ))}
          </select>
        )}
        {(filterText || filterKind || filterSource) && (
          <button
            type="button"
            onClick={() => {
              setFilterText("");
              setFilterKind("");
              setFilterSource("");
            }}
            className="text-fg-subtle hover:text-fg-default underline"
          >
            reset
          </button>
        )}
      </div>

      {error && (
        <div className="text-danger-fg text-[11px]" role="alert">
          {error}
        </div>
      )}

      {!findings && <p className="text-fg-muted text-[11px]">Loading…</p>}

      {findings && filtered && filtered.length === 0 && (
        <p className="text-fg-muted text-[11px] italic">
          {total === 0
            ? "Inbox is empty — bots haven't surfaced anything new."
            : "No findings match the current filters."}
        </p>
      )}

      {filtered &&
        filtered.map((f) => {
          const expanded = expandedId === f.filename;
          return (
            <article
              key={f.filename}
              className="rounded-md border border-border-subtle bg-surface-0 px-3 py-2 space-y-1"
            >
              <header className="flex items-baseline gap-2 flex-wrap">
                <button
                  type="button"
                  onClick={() =>
                    setExpandedId(expanded ? null : f.filename)
                  }
                  className="text-[13px] font-medium text-fg-default text-left hover:underline"
                  aria-expanded={expanded}
                >
                  {f.title || f.filename}
                </button>
                {f.kind && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-warning-soft/30 text-warning-fg">
                    kind:{f.kind}
                  </span>
                )}
                {f.source_bot && (
                  <span className="text-[10px] px-1.5 py-0.5 rounded bg-accent-soft/30 text-accent">
                    {f.source_bot}
                  </span>
                )}
                <span className="ml-auto text-[10px] text-fg-muted">
                  {formatRelative(f.modified_at)}
                </span>
                <button
                  type="button"
                  className="text-danger-fg hover:text-danger text-[11px] underline disabled:opacity-50"
                  onClick={() => void onArchive(f)}
                  disabled={busyId === f.filename}
                >
                  {busyId === f.filename ? "…" : "archive"}
                </button>
              </header>
              {f.description && (
                <p className="text-[12px] text-fg-default">{f.description}</p>
              )}
              {(f.tags?.length ?? 0) > 0 && (
                <div className="flex flex-wrap gap-1">
                  {f.tags!.map((t) => (
                    <span
                      key={t}
                      className="text-[10px] px-1.5 py-0.5 rounded bg-surface-1 text-fg-muted"
                    >
                      {t}
                    </span>
                  ))}
                </div>
              )}
              {expanded && f.body && (
                <div className="pt-1 border-t border-border-subtle">
                  <MarkdownText value={f.body} size="sm" />
                  {f.body_truncated && (
                    <p className="text-[10px] text-fg-subtle italic mt-1">
                      (preview truncated — full file at{" "}
                      <code className="not-italic">{f.path}</code>)
                    </p>
                  )}
                </div>
              )}
              <p className="text-[10px] text-fg-subtle font-mono break-all">
                {f.path}
              </p>
            </article>
          );
        })}
    </div>
  );
}
