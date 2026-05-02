import { useEffect, useMemo, useRef, useState } from "react";
import { Virtuoso, type VirtuosoHandle } from "react-virtuoso";
import { ChevronDownIcon } from "@radix-ui/react-icons";

import { IconButton, Input } from "@/components/ui";
import { formatBytes } from "@/lib/format";
import { useRunStore } from "@/store/run";

interface Props {
  runId: string;
  // Imperative log subscription wired by RunView. The panel calls
  // subscribe on mount (when this is the active tab) and unsubscribe
  // on unmount so the WS pays bandwidth only when logs are visible.
  subscribeLogs: (fromOffset?: number) => void;
  unsubscribeLogs: () => void;
  onCollapse?: () => void;
}

// Level glyphs used by the iterion leveled logger (pkg/log/log.go).
// Used to drive the level filter chips and per-line styling.
const LEVEL_GLYPHS: Array<{ key: string; emoji: string; label: string; cls: string }> = [
  { key: "error", emoji: "❌", label: "error", cls: "text-danger-fg" },
  // The iterion logger emits "⚠️ " (with NBSP padding) for warn; we
  // match against the plain warning sign so the chip works either way.
  { key: "warn", emoji: "⚠️", label: "warn", cls: "text-warning-fg" },
  { key: "info", emoji: "ℹ️", label: "info", cls: "text-info-fg" },
  { key: "debug", emoji: "🔍", label: "debug", cls: "text-fg-muted" },
  { key: "trace", emoji: "🔬", label: "trace", cls: "text-fg-subtle" },
];

const BLOCK_INDENT = "         │ ";

interface AnnotatedLine {
  // Index in the lines[] array, used as a stable key for Virtuoso.
  idx: number;
  text: string;
  // Inferred level (from the emoji prefix on the first byte after the
  // timestamp). Null for continuation lines that inherit their parent's
  // level — but for filtering we treat them as belonging to the most
  // recent header.
  level: string | null;
  isContinuation: boolean;
}

export default function RunLogPanel({ runId, subscribeLogs, unsubscribeLogs, onCollapse }: Props) {
  const log = useRunStore((s) => s.log);
  const [search, setSearch] = useState("");
  const [activeLevels, setActiveLevels] = useState<Set<string>>(() => new Set());
  const [followTail, setFollowTail] = useState(true);
  const virtuosoRef = useRef<VirtuosoHandle>(null);

  // Subscribe on mount (this component is mounted only when the Logs
  // tab is active) and unsubscribe on unmount.
  useEffect(() => {
    subscribeLogs();
    return () => {
      unsubscribeLogs();
    };
  }, [subscribeLogs, unsubscribeLogs]);

  // Annotate lines with their inferred level. Continuation lines (from
  // LogBlock) inherit the level of the most recent header so a level
  // filter doesn't strand a multi-line block's body.
  const annotated = useMemo<AnnotatedLine[]>(() => {
    if (!log.text) return [];
    // Slice off a trailing empty line caused by the final "\n".
    const raw = log.text.endsWith("\n") ? log.text.slice(0, -1) : log.text;
    const split = raw.split("\n");
    const out: AnnotatedLine[] = new Array(split.length);
    let lastLevel: string | null = null;
    for (let i = 0; i < split.length; i++) {
      const t = split[i] ?? "";
      if (t.startsWith(BLOCK_INDENT)) {
        out[i] = { idx: i, text: t, level: lastLevel, isContinuation: true };
      } else {
        const lvl = inferLevel(t);
        if (lvl) lastLevel = lvl;
        out[i] = { idx: i, text: t, level: lvl, isContinuation: false };
      }
    }
    return out;
  }, [log.text]);

  const filtered = useMemo(() => {
    const query = search.trim().toLowerCase();
    if (activeLevels.size === 0 && !query) return annotated;
    return annotated.filter((line) => {
      if (activeLevels.size > 0) {
        const lvl = line.level ?? "";
        if (!activeLevels.has(lvl)) return false;
      }
      if (query && !line.text.toLowerCase().includes(query)) return false;
      return true;
    });
  }, [annotated, search, activeLevels]);

  // Auto-tail when followTail is on. Identical pattern to EventLog.
  useEffect(() => {
    if (!followTail) return;
    if (filtered.length === 0) return;
    virtuosoRef.current?.scrollToIndex({
      index: filtered.length - 1,
      align: "end",
      behavior: "auto",
    });
  }, [filtered.length, followTail]);

  const toggleLevel = (lvl: string) => {
    setActiveLevels((prev) => {
      const next = new Set(prev);
      if (next.has(lvl)) next.delete(lvl);
      else next.add(lvl);
      return next;
    });
  };

  const lineCount = annotated.length;
  const droppedBytes = log.start;
  const totalBytes = log.total;

  return (
    <div className="h-full flex flex-col bg-surface-1 min-h-0">
      <div className="px-3 py-1.5 border-b border-border-default flex flex-wrap items-center gap-2 text-[11px]">
        <span className="font-medium text-fg-muted">Logs</span>
        <span className="text-fg-subtle">
          {filtered.length} / {lineCount} lines · {formatBytes(totalBytes)}
        </span>
        {droppedBytes > 0 && (
          <span
            className="text-warning-fg text-[10px]"
            title={`${formatBytes(droppedBytes)} of older output rolled out of the in-memory tail; download the full log to inspect.`}
          >
            +{formatBytes(droppedBytes)} older evicted
          </span>
        )}
        {log.terminated && (
          <span className="text-fg-subtle text-[10px] italic">stream ended</span>
        )}
        {!log.subscribed && !log.terminated && (
          <span className="text-fg-subtle text-[10px] italic">connecting…</span>
        )}
        <div className="flex-1 min-w-[140px] max-w-[320px]">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search log…"
            size="sm"
            leadingIcon={<span className="text-[10px]">⌕</span>}
          />
        </div>
        <a
          href={`/api/runs/${encodeURIComponent(runId)}/log`}
          target="_blank"
          rel="noreferrer"
          className="text-[10px] text-fg-subtle hover:text-fg-default underline"
          title="Open the full log in a new tab"
        >
          download
        </a>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={followTail}
            onChange={(e) => setFollowTail(e.target.checked)}
            className="accent-accent"
          />
          Follow tail
        </label>
        {onCollapse && (
          <IconButton
            label="Hide log panel"
            size="sm"
            variant="ghost"
            onClick={onCollapse}
          >
            <ChevronDownIcon />
          </IconButton>
        )}
      </div>
      <div className="px-3 py-1 border-b border-border-default flex flex-wrap gap-1">
        {LEVEL_GLYPHS.map((g) => {
          const isActive = activeLevels.has(g.key);
          return (
            <button
              key={g.key}
              type="button"
              onClick={() => toggleLevel(g.key)}
              className={`text-[10px] px-1.5 py-0.5 rounded border transition-colors ${
                isActive
                  ? `bg-surface-2 ${g.cls} border-accent`
                  : `bg-surface-1 border-border-default ${g.cls} hover:text-fg-default`
              }`}
              title={`Filter to ${g.label} only`}
            >
              {g.emoji} {g.label}
            </button>
          );
        })}
      </div>
      <div className="flex-1 min-h-0 px-3 py-1">
        {filtered.length === 0 ? (
          <div className="text-fg-subtle py-2 text-[11px]">
            {lineCount === 0
              ? log.subscribed
                ? "Waiting for log output…"
                : "No log captured."
              : "No log lines match."}
          </div>
        ) : (
          <Virtuoso
            ref={virtuosoRef}
            className="h-full"
            data={filtered}
            followOutput={followTail ? "auto" : false}
            atBottomStateChange={(atBottom) => {
              if (!atBottom && followTail) setFollowTail(false);
            }}
            itemContent={(_, line) => <LogLineRow line={line} />}
            computeItemKey={(_, line) => line.idx}
          />
        )}
      </div>
    </div>
  );
}

function LogLineRow({ line }: { line: AnnotatedLine }) {
  const cls = line.level
    ? LEVEL_GLYPHS.find((g) => g.key === line.level)?.cls ?? "text-fg-default"
    : "text-fg-default";
  return (
    <div
      className={`font-mono text-[10px] whitespace-pre overflow-x-auto py-0.5 ${cls}`}
    >
      {line.text || " "}
    </div>
  );
}

function inferLevel(text: string): string | null {
  // The logger writes lines as "HH:MM:SS <emoji> <message>". The
  // timestamp is fixed-width (8 chars + space), then the emoji.
  if (text.length < 10) return null;
  const after = text.slice(9);
  for (const g of LEVEL_GLYPHS) {
    if (after.startsWith(g.emoji)) return g.key;
  }
  return null;
}
