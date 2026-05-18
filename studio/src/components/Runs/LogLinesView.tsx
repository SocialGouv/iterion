import { forwardRef, memo, useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import type { ComponentPropsWithRef } from "react";
import type { Components, ScrollerProps } from "react-virtuoso";
import { Virtuoso, type VirtuosoHandle } from "react-virtuoso";
import { ChevronDownIcon, MixerHorizontalIcon } from "@radix-ui/react-icons";
import { useShallow } from "zustand/react/shallow";

import { apiURL } from "@/api/runs";
import { IconButton, Input, Popover } from "@/components/ui";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { formatBytes } from "@/lib/format";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import { selectInFlightTools, useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

import { ActiveFooter } from "./ActiveFooter";
import { LogSidePanel } from "./LogSidePanel";

const SIDE_PANEL_FLAG_KEY = "iterion.logs.sidePanel";

interface Props {
  runId: string;
  // Imperative log subscription. Both the global RunLogPanel and the
  // per-node Logs tab in NodeDetailPanel call subscribe on mount and
  // unsubscribe on unmount. The hook layer ref-counts these calls so
  // the WS subscription stays active until the last consumer detaches.
  subscribeLogs: (fromOffset?: number) => void;
  unsubscribeLogs: () => void;
  // Optional per-(node, iteration) filter. When filterNodeId is set,
  // only lines whose body starts with `[<NodeID>#<iter>/` or
  // `[<NodeID>#<iter>]` (after the timestamp+emoji prefix) are kept;
  // continuation lines inherit their parent header's decision so
  // multi-line tool input blocks stay grouped. Engine-level lines
  // without a node tag fall out of the view by design.
  filterNodeId?: string | null;
  filterIteration?: number | null;
  // Controls whether the header includes the "Logs" title.
  // The bottom-panel wrapper sets this to true; the inline tab in
  // NodeDetailPanel suppresses it (the tab label already says "Logs").
  showTitle?: boolean;
  onCollapse?: () => void;
  // Absolute byte offset to clamp the displayed log to. When set,
  // only bytes with absolute position < clampToBytes appear — used
  // by the time-travel scrubber / replay to show "what the log
  // looked like at this point in the run". Null/undefined disables
  // clamping (show everything in the buffer). Offsets below
  // log.start fall outside the in-memory window and render as empty
  // — that part of the log has been evicted from the 1 MB ring.
  clampToBytes?: number | null;
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

// Bytes between the start of a log line and the [<NodeID>#<iter>/...]
// tag: 8-char "HH:MM:SS" timestamp + 1 separator. Anything before this
// is the timestamp; the tag lives further right after the emoji.
const LOG_TAG_MIN_OFFSET = 9;

// Slack the bottom-detection threshold so dynamic-height row reflows
// don't transiently report "not at bottom" while followOutput re-aligns.
const AT_BOTTOM_THRESHOLD_PX = 48;

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

// A LogItem is one Virtuoso row. Continuation lines are folded under
// their parent header into a single "block" item that the user can
// expand/collapse; lines without continuation render as plain rows.
type LogItem =
  | { kind: "line"; line: AnnotatedLine; key: number }
  | { kind: "block"; header: AnnotatedLine; body: AnnotatedLine[]; key: number };

function groupLog(lines: AnnotatedLine[]): LogItem[] {
  const out: LogItem[] = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    if (line.isContinuation) {
      // Orphan continuation (parent header filtered out by something
      // upstream of grouping) — render as a standalone line so it stays
      // visible.
      out.push({ kind: "line", line, key: line.idx });
      continue;
    }
    const body: AnnotatedLine[] = [];
    let j = i + 1;
    while (j < lines.length && lines[j]!.isContinuation) {
      body.push(lines[j]!);
      j++;
    }
    if (body.length === 0) {
      out.push({ kind: "line", line, key: line.idx });
    } else {
      out.push({ kind: "block", header: line, body, key: line.idx });
      i = j - 1;
    }
  }
  return out;
}

export default function LogLinesView({
  runId,
  subscribeLogs,
  unsubscribeLogs,
  filterNodeId = null,
  filterIteration = null,
  showTitle = true,
  onCollapse,
  clampToBytes = null,
}: Props) {
  const log = useRunStore((s) => s.log);
  // Drives the "thinking" footer. When filtering to a specific node,
  // we must scope this to *that* execution — otherwise a sibling node
  // running in parallel keeps the footer visible after the selected
  // node finishes, making it look like the node is still working.
  const active = useRunStore((s) => {
    if (s.snapshot?.run.status !== "running") return false;
    if (filterNodeId) {
      const iter = filterIteration ?? 0;
      for (const e of s.executionsById.values()) {
        if (
          e.ir_node_id === filterNodeId &&
          e.loop_iteration === iter &&
          e.status === "running"
        ) {
          return true;
        }
      }
      return false;
    }
    for (const e of s.executionsById.values()) {
      if (e.status === "running") return true;
    }
    return false;
  });
  // ActiveFooter partitions this list: a synchronous tool (Bash,
  // Read, Edit, …) commands the footer with "Running <tool>"; agentic
  // tools (Agent/Task) are mostly an LLM wait under the hood and are
  // left to the side panel's count so the footer can keep showing the
  // random-words "thinking" loader.
  const inFlightTools = useRunStore(
    useShallow((s) => selectInFlightTools(s, filterNodeId, filterIteration)),
  );
  const [search, setSearch] = useState("");
  const [activeLevels, setActiveLevels] = useState<Set<string>>(() => new Set());
  const [followTail, setFollowTail] = useState(true);
  // Word wrap toggle: when off (default), long lines extend horizontally
  // and the Virtuoso scroller carries a single horizontal scrollbar at
  // the bottom — replaces the per-row scrollbars the original code
  // grew on every line. When on, lines wrap to the next visible line
  // and no horizontal scroll is needed.
  const [wordWrap, setWordWrap] = useState(false);
  // Side panel (task list + pending agents) toggle. Persisted globally so
  // the operator's choice survives reloads and follows them across runs.
  const [showSidePanel, setShowSidePanelState] = useState<boolean>(() =>
    readBooleanFlag(SIDE_PANEL_FLAG_KEY, false),
  );
  const setShowSidePanel = (next: boolean) => {
    setShowSidePanelState(next);
    writeBooleanFlag(SIDE_PANEL_FLAG_KEY, next);
  };
  const virtuosoRef = useRef<VirtuosoHandle>(null);
  const isScrollingRef = useRef<boolean>(false);
  const disabledByScrollRef = useRef<boolean>(false);

  // Subscribe on mount and unsubscribe on unmount. The hook
  // ref-counts these so multiple mounted views share one WS
  // subscription.
  useEffect(() => {
    subscribeLogs();
    return () => {
      unsubscribeLogs();
    };
  }, [subscribeLogs, unsubscribeLogs]);

  // Annotate lines with their inferred level. Continuation lines (from
  // LogBlock) inherit the level of the most recent header so a level
  // filter doesn't strand a multi-line block's body.
  //
  // When clampToBytes is set, slice the buffer to the absolute byte
  // position the time-travel scrubber asks for (events[scrubSeq].log_
  // offset on the parent side). Bytes BEFORE the ring's lower bound
  // (log.start) have been evicted from the 1 MB window — those are
  // silently absent until we wire a /api/runs/{id}/log fetch fallback.
  const annotated = useMemo<AnnotatedLine[]>(() => {
    if (!log.text) return [];
    let body = log.text;
    if (clampToBytes !== null && clampToBytes !== undefined) {
      const rel = clampToBytes - log.start;
      if (rel <= 0) return [];
      if (rel < body.length) body = body.slice(0, rel);
    }
    const raw = body.endsWith("\n") ? body.slice(0, -1) : body;
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
  }, [log.text, log.start, clampToBytes]);

  // Per-(node, iteration) pre-filter. Backends prefix every node-bound
  // line as [NodeID#iter/component]; we scan past the timestamp+emoji
  // and check for that prefix. Continuation lines (BLOCK_INDENT) are
  // detected by the annotator above and inherit their parent header's
  // match decision so multi-line tool blocks stay grouped.
  //
  // When filtering, the `NodeID#iter/` segment is redundant — the
  // panel is already scoped to that exact (node, iteration). We
  // rewrite the tag to keep only the component (e.g. "claude-code",
  // "claw") so the user sees `[claude-code]` instead of
  // `[discover_outdated#0/claude-code]`. The component itself is
  // meaningful (distinguishes engine/runtime/delegate stripes), so we
  // keep it.
  const nodeFiltered = useMemo<AnnotatedLine[]>(() => {
    if (!filterNodeId) return annotated;
    const tagPrefix = `[${filterNodeId}#${filterIteration ?? 0}/`;
    const tagPrefixLen = tagPrefix.length;
    const out: AnnotatedLine[] = [];
    let headerMatched = false;
    for (const line of annotated) {
      if (line.isContinuation) {
        if (headerMatched) out.push(line);
        continue;
      }
      const idx = line.text.indexOf("[");
      headerMatched = idx >= LOG_TAG_MIN_OFFSET && line.text.startsWith(tagPrefix, idx);
      if (headerMatched) {
        const stripped =
          line.text.slice(0, idx + 1) + line.text.slice(idx + tagPrefixLen);
        out.push({ ...line, text: stripped });
      }
    }
    return out;
  }, [annotated, filterNodeId, filterIteration]);

  // Custom Scroller for Virtuoso so we can opt in to horizontal scroll
  // alongside the default vertical scroll. Rows render at their natural
  // width (whitespace-pre, min-w-max), the Scroller carries the
  // horizontal scrollbar at the bottom of the panel — one bar for the
  // whole list instead of one per row.
  const virtuosoComponents = useMemo<Components<LogItem>>(
    () => ({
      Footer: () => (
        <ActiveFooter inFlightTools={inFlightTools} active={active} />
      ),
      Scroller: wordWrap ? WrapScroller : HScroller,
    }),
    [active, inFlightTools, wordWrap],
  );

  // useDeferredValue keeps keystrokes responsive on runs with very long
  // logs (50k+ lines): the input updates immediately while the filtered
  // list lags one frame behind, so typing doesn't stutter while React
  // re-runs the O(n) text scan.
  const deferredSearch = useDeferredValue(search);
  const filtered = useMemo(() => {
    const query = deferredSearch.trim().toLowerCase();
    if (activeLevels.size === 0 && !query) return nodeFiltered;
    return nodeFiltered.filter((line) => {
      if (activeLevels.size > 0) {
        const lvl = line.level ?? "";
        if (!activeLevels.has(lvl)) return false;
      }
      if (query && !line.text.toLowerCase().includes(query)) return false;
      return true;
    });
  }, [nodeFiltered, deferredSearch, activeLevels]);

  // Group continuation lines under their header into foldable blocks.
  // When the user is searching, fall back to flat rows so every match is
  // visible — collapsed blocks would hide hits inside the body.
  const items = useMemo<LogItem[]>(() => {
    if (deferredSearch.trim()) {
      return filtered.map((line) => ({
        kind: "line" as const,
        line,
        key: line.idx,
      }));
    }
    return groupLog(filtered);
  }, [filtered, deferredSearch]);

  useEffect(() => {
    if (followTail && items.length > 0) {
      virtuosoRef.current?.scrollToIndex({
        index: items.length - 1,
        align: "end",
        behavior: "auto",
      });
    }
  }, [followTail, items.length]);

  const handleToggleFollow = (next: boolean) => {
    disabledByScrollRef.current = false;
    setFollowTail(next);
    if (next && items.length > 0) {
      virtuosoRef.current?.scrollToIndex({
        index: items.length - 1,
        align: "end",
        behavior: "auto",
      });
    }
  };

  const toggleLevel = (lvl: string) => {
    setActiveLevels((prev) => {
      const next = new Set(prev);
      if (next.has(lvl)) next.delete(lvl);
      else next.add(lvl);
      return next;
    });
  };

  const lineCount = filterNodeId ? nodeFiltered.length : annotated.length;
  const droppedBytes = log.start;
  const totalBytes = log.total;
  const isFiltered = filterNodeId != null;

  return (
    <div className="h-full flex flex-col bg-surface-1 min-h-0">
      <div className="px-3 py-1.5 border-b border-border-default flex flex-wrap items-center gap-2 text-[11px]">
        {showTitle && <span className="font-medium text-fg-muted">Logs</span>}
        <span className="text-fg-subtle">
          {filtered.length} / {lineCount} lines
          {!isFiltered && <> · {formatBytes(totalBytes)}</>}
        </span>
        {!isFiltered && droppedBytes > 0 && (
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
        <button
          type="button"
          onClick={() => {
            void (async () => {
              try {
                if (isFiltered) {
                  // Copy in-memory filtered slice — the per-node view
                  // doesn't have a server-side counterpart.
                  const text = filtered.map((l) => l.text).join("\n");
                  await navigator.clipboard.writeText(text);
                  useUIStore.getState().addToast("Log copied to clipboard", "success");
                  return;
                }
                const res = await fetch(apiURL(`/runs/${encodeURIComponent(runId)}/log`), {
                  credentials: "include",
                });
                if (!res.ok) throw new Error(`HTTP ${res.status}`);
                const text = await res.text();
                await navigator.clipboard.writeText(text);
                useUIStore.getState().addToast("Log copied to clipboard", "success");
              } catch (err) {
                console.error("[LogLinesView] copy log failed:", err);
                useUIStore.getState().addToast("Copy failed", "error");
              }
            })();
          }}
          className="text-[10px] text-fg-subtle hover:text-fg-default underline"
          title={
            isFiltered
              ? "Copy the visible (per-node) log lines to the clipboard"
              : "Copy the full run.log to the clipboard"
          }
        >
          copy
        </button>
        {!isFiltered && (
          <button
            type="button"
            onClick={() => {
              void (async () => {
                try {
                  const res = await fetch(apiURL(`/runs/${encodeURIComponent(runId)}/log`), {
                    credentials: "include",
                  });
                  if (!res.ok) throw new Error(`HTTP ${res.status}`);
                  if (isDesktop()) {
                    const text = await res.text();
                    await desktop.saveTextFile(`${runId}.log`, text);
                    return;
                  }
                  const blob = await res.blob();
                  const url = URL.createObjectURL(blob);
                  const a = document.createElement("a");
                  a.href = url;
                  a.download = `${runId}.log`;
                  document.body.appendChild(a);
                  a.click();
                  a.remove();
                  setTimeout(() => URL.revokeObjectURL(url), 1000);
                } catch (err) {
                  console.error("[LogLinesView] download log failed:", err);
                }
              })();
            }}
            className="text-[10px] text-fg-subtle hover:text-fg-default underline"
            title="Save the full run.log to a file"
          >
            download
          </button>
        )}
        <Popover
          side="bottom"
          align="end"
          contentClassName="p-1 min-w-[140px]"
          trigger={
            <button
              type="button"
              title={
                activeLevels.size > 0
                  ? `${activeLevels.size} level filter(s) active`
                  : "Filter by level"
              }
              className={`text-[10px] px-1.5 py-0.5 rounded border inline-flex items-center gap-1 transition-colors ${
                activeLevels.size > 0
                  ? "bg-surface-2 text-fg-default border-accent"
                  : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
              }`}
            >
              <MixerHorizontalIcon className="w-3 h-3" />
              Levels
              {activeLevels.size > 0 && (
                <span className="font-mono text-[9px] px-1 rounded bg-accent/20 text-accent-fg">
                  {activeLevels.size}
                </span>
              )}
            </button>
          }
        >
          <div className="flex flex-col gap-0.5 p-1">
            {LEVEL_GLYPHS.map((g) => {
              const isActive = activeLevels.has(g.key);
              return (
                <button
                  key={g.key}
                  type="button"
                  onClick={() => toggleLevel(g.key)}
                  className={`text-[11px] px-2 py-1 rounded border text-left transition-colors ${
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
            {activeLevels.size > 0 && (
              <button
                type="button"
                onClick={() => setActiveLevels(new Set())}
                className="text-[10px] text-fg-subtle hover:text-fg-default mt-1 px-2 py-0.5 text-left"
              >
                Clear filters
              </button>
            )}
          </div>
        </Popover>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={showSidePanel}
            onChange={(e) => setShowSidePanel(e.target.checked)}
            className="accent-accent"
          />
          Tasks
        </label>
        <label className="inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={wordWrap}
            onChange={(e) => setWordWrap(e.target.checked)}
            className="accent-accent"
          />
          Wrap
        </label>
        <label className="inline-flex items-center gap-1.5 cursor-pointer">
          <input
            type="checkbox"
            checked={followTail}
            onChange={(e) => handleToggleFollow(e.target.checked)}
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
      <div className="flex-1 min-h-0 flex">
        <div className="flex-1 min-w-0 min-h-0 px-3 py-1">
          {filtered.length === 0 ? (
            <div className="text-fg-subtle py-2 text-[11px]">
              {emptyMessage(lineCount, isFiltered, log.subscribed)}
            </div>
          ) : (
            <Virtuoso
              ref={virtuosoRef}
              className="h-full"
              data={items}
              initialTopMostItemIndex={
                followTail
                  ? { index: items.length - 1, align: "end" }
                  : 0
              }
              followOutput={followTail ? "auto" : false}
              atBottomThreshold={AT_BOTTOM_THRESHOLD_PX}
              isScrolling={(s) => {
                isScrollingRef.current = s;
              }}
              atBottomStateChange={(atBottom) => {
                if (!atBottom && followTail && isScrollingRef.current) {
                  disabledByScrollRef.current = true;
                  setFollowTail(false);
                } else if (
                  atBottom &&
                  !followTail &&
                  disabledByScrollRef.current
                ) {
                  disabledByScrollRef.current = false;
                  setFollowTail(true);
                }
              }}
              itemContent={(_, item) =>
                item.kind === "line" ? (
                  <LogLineRow line={item.line} wrap={wordWrap} />
                ) : (
                  <LogBlockRow
                    header={item.header}
                    body={item.body}
                    wrap={wordWrap}
                  />
                )
              }
              computeItemKey={(_, item) => item.key}
              components={virtuosoComponents}
            />
          )}
        </div>
        {showSidePanel && (
          <div className="flex-none w-72 border-l border-border-default bg-surface-1">
            <LogSidePanel
              filterNodeId={filterNodeId ?? null}
              filterIteration={filterIteration ?? null}
            />
          </div>
        )}
      </div>
    </div>
  );
}

// HScroller swaps Virtuoso's default Scroller for one that also lets
// horizontal overflow scroll. Rows below opt into `min-w-max` so the
// Scroller's content extends past the viewport and a single horizontal
// bar appears at the bottom. The `data-virtuoso-scroller` attribute is
// preserved so Virtuoso's internal scroll-position tracking keeps
// working.
const HScroller = forwardRef<HTMLDivElement, ScrollerProps>(
  function HScroller({ style, ...rest }, ref) {
    return (
      <div
        ref={ref}
        {...(rest as ComponentPropsWithRef<"div">)}
        style={{ ...style, overflowX: "auto" }}
      />
    );
  },
);

const WrapScroller = forwardRef<HTMLDivElement, ScrollerProps>(
  function WrapScroller({ style, ...rest }, ref) {
    return (
      <div
        ref={ref}
        {...(rest as ComponentPropsWithRef<"div">)}
        style={{ ...style, overflowX: "hidden" }}
      />
    );
  },
);

// Memoised so a streaming run that appends new log items (which forces
// the parent Virtuoso into a re-render) doesn't re-render every visible
// LogLineRow whose props are reference-stable. The store builds the
// items array incrementally, so identity is preserved for unchanged
// lines.
const LogLineRow = memo(function LogLineRow({
  line,
  wrap,
}: {
  line: AnnotatedLine;
  wrap: boolean;
}) {
  const cls = line.level
    ? LEVEL_GLYPHS.find((g) => g.key === line.level)?.cls ?? "text-fg-default"
    : "text-fg-default";
  // No-wrap mode: keep `whitespace-pre` + `min-w-max` so the row
  // contributes to the Scroller's horizontal extent (single global
  // scrollbar). Wrap mode: pre-wrap with word-break so long tokens
  // (paths, tool inputs) don't bust the viewport.
  const widthCls = wrap
    ? "whitespace-pre-wrap break-all"
    : "whitespace-pre min-w-max";
  return (
    <div className={`font-mono text-[10px] py-0.5 ${widthCls} ${cls}`}>
      {line.text || " "}
    </div>
  );
});

const LogBlockRow = memo(function LogBlockRow({
  header,
  body,
  wrap,
}: {
  header: AnnotatedLine;
  body: AnnotatedLine[];
  wrap: boolean;
}) {
  const [open, setOpen] = useState(false);
  const cls = header.level
    ? LEVEL_GLYPHS.find((g) => g.key === header.level)?.cls ?? "text-fg-default"
    : "text-fg-default";
  const bodyWidthCls = wrap
    ? "whitespace-pre-wrap break-all"
    : "whitespace-pre min-w-max";
  const toggle = () => setOpen((v) => !v);
  return (
    <div className={`font-mono text-[10px] py-0.5 ${cls}`}>
      <div
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            toggle();
          }
        }}
        title={open ? "Replier le corps" : "Déplier le corps"}
        className="flex items-baseline gap-2 pr-3 rounded cursor-pointer hover:bg-surface-2"
      >
        <div className={`flex-1 min-w-0 ${bodyWidthCls}`}>
          {header.text || " "}
        </div>
        <span className="shrink-0 text-fg-subtle">
          {open ? "▾" : `▸ +${body.length}`}
        </span>
      </div>
      {open &&
        body.map((line) => (
          <div key={line.idx} className={bodyWidthCls}>
            {line.text || " "}
          </div>
        ))}
    </div>
  );
});

function inferLevel(text: string): string | null {
  if (text.length < 10) return null;
  const after = text.slice(9);
  for (const g of LEVEL_GLYPHS) {
    if (after.startsWith(g.emoji)) return g.key;
  }
  return null;
}

function emptyMessage(
  lineCount: number,
  isFiltered: boolean,
  subscribed: boolean,
): string {
  if (lineCount > 0) return "No log lines match.";
  if (isFiltered) return "No log lines tagged with this node yet.";
  if (subscribed) return "Waiting for log output…";
  return "No log captured.";
}
