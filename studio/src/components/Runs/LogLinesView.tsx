import { forwardRef, memo, useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import type { ComponentPropsWithRef } from "react";
import type { Components, ScrollerProps } from "react-virtuoso";
import { Virtuoso, type VirtuosoHandle } from "react-virtuoso";
import { ChevronDownIcon, MixerHorizontalIcon } from "@radix-ui/react-icons";
import { useShallow } from "zustand/react/shallow";

import { apiURL } from "@/api/runs";
import { Button, Checkbox, IconButton, Input, Popover } from "@/components/ui";
import { useToggleSet } from "@/hooks/useToggleSet";
import { clickableRowProps } from "@/lib/a11y";
import { desktop, isDesktop } from "@/lib/desktopBridge";
import { formatBytes } from "@/lib/format";
import { downloadBlob } from "@/lib/download";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import { selectInFlightTools, useRunStore } from "@/store/run";
import { clampLogBody } from "@/store/run/logBuffer";
import { useTabsStore } from "@/store/tabs";
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
  const { set: activeLevels, toggle: toggleLevel, clear: clearActiveLevels } = useToggleSet<string>();
  const [followTail, setFollowTail] = useState(true);
  // Re-enforce follow-tail when this run is (re)opened via navigation
  // (board / runs list / deep-link). RunsTabsView bumps this nonce on
  // open but NOT on a plain tab-strip click — so returning to a tab you
  // had scrolled up in keeps your position. See store/tabs runOpenNonce.
  const runOpenNonce = useTabsStore((s) => s.runOpenNonce[runId] ?? 0);
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

  // On a navigation-driven (re)open of this run (RunsTabsView bumps
  // runOpenNonce — board / runs list / deep-link, NOT a plain tab-strip
  // click), snap back to following the tail. Also runs on mount, where
  // followTail already starts true so it's a no-op.
  useEffect(() => {
    disabledByScrollRef.current = false;
    setFollowTail(true);
  }, [runOpenNonce]);

  // Subscribe on mount and unsubscribe on unmount. The hook
  // ref-counts these so multiple mounted views share one WS
  // subscription.
  useEffect(() => {
    subscribeLogs();
    return () => {
      unsubscribeLogs();
    };
  }, [subscribeLogs, unsubscribeLogs]);

  // Evicted-head fallback for the replay scrubber. The in-memory buffer
  // is a bounded tail (MAX_LOG_BYTES): on a log larger than the cap the
  // head [0, log.start) is trimmed away, and a scrub position below
  // log.start used to render as an empty panel mislabelled "No log
  // captured." Fetch that prefix once from the authoritative persisted
  // log (run.log on a filesystem store, run_logs chunks on Mongo) and
  // cache it in the run store; the cache is keyed on log.start so a
  // further live trim triggers a refetch of the widened gap.
  const prefix = log.prefix;
  const setLogPrefix = useRunStore((s) => s.setLogPrefix);
  const clamping = clampToBytes !== null && clampToBytes !== undefined;
  const needPrefixFetch =
    clamping && log.start > 0 && (!prefix || prefix.until !== log.start);
  // The error is keyed by the `until` it failed for, so a later trim
  // (a new fetch target) supersedes it without a synchronous state
  // reset inside the effect.
  const [prefixError, setPrefixError] = useState<{
    until: number;
    message: string;
  } | null>(null);
  useEffect(() => {
    if (!needPrefixFetch) return;
    const until = log.start;
    let cancelled = false;
    void (async () => {
      try {
        const res = await fetch(
          apiURL(`/runs/${encodeURIComponent(runId)}/log?from=0&until=${until}`),
          { credentials: "include" },
        );
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const text = await res.text();
        if (!cancelled) setLogPrefix({ until, text });
      } catch (err) {
        console.error("[LogLinesView] evicted-log prefix fetch failed:", err);
        if (!cancelled) setPrefixError({ until, message: String(err) });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [needPrefixFetch, log.start, runId, setLogPrefix]);
  const prefixState: PrefixState = !clamping || log.start === 0
    ? "none"
    : prefix && prefix.until === log.start
      ? "ready"
      : prefixError && prefixError.until === log.start
        ? "error"
        : "loading";

  // Annotate lines with their inferred level. Continuation lines (from
  // LogBlock) inherit the level of the most recent header so a level
  // filter doesn't strand a multi-line block's body.
  //
  // When clampToBytes is set, clampLogBody slices the buffer to the
  // absolute byte position the time-travel scrubber asks for
  // (events[scrubSeq].log_offset on the parent side), byte-accurately,
  // stitching in the fetched evicted-head prefix when the position
  // falls below the in-memory window.
  const annotated = useMemo<AnnotatedLine[]>(() => {
    const body = clampLogBody(log.text, log.start, prefix, clampToBytes);
    if (!body) return [];
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
  }, [log.text, log.start, prefix, clampToBytes]);

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
    // The log line tag is `[<node>#<task.Iteration>/<component>]`, where
    // task.Iteration is the backend's raw scalar iteration. The exec's
    // `loop_iteration` we filter on is derived from iteration_path (nested-loop
    // aware), so the two numbers can diverge under recovery/nested loops — an
    // exact `#<iter>/` match then yields ZERO lines and the per-node Logs tab
    // reads as empty even though the node is streaming. So accumulate BOTH the
    // iteration-scoped lines and the node-only lines (any iteration), and fall
    // back to node-only when the scoped set is empty — the tab is then never
    // wrongly blank; the iteration stays visible in the fallback so the
    // operator can see which attempt each line belongs to.
    const iterPrefix = `[${filterNodeId}#${filterIteration ?? 0}/`;
    const iterPrefixLen = iterPrefix.length;
    const nodePrefix = `[${filterNodeId}#`;
    const iterOut: AnnotatedLine[] = [];
    const nodeOut: AnnotatedLine[] = [];
    let iterMatched = false;
    let nodeMatched = false;
    for (const line of annotated) {
      if (line.isContinuation) {
        if (iterMatched) iterOut.push(line);
        if (nodeMatched) nodeOut.push(line);
        continue;
      }
      const idx = line.text.indexOf("[");
      const tagged = idx >= LOG_TAG_MIN_OFFSET;
      iterMatched = tagged && line.text.startsWith(iterPrefix, idx);
      nodeMatched = tagged && line.text.startsWith(nodePrefix, idx);
      if (iterMatched) {
        // Scoped view: the `<node>#<iter>/` segment is redundant — strip it to
        // the bare `[<component>]`.
        const stripped =
          line.text.slice(0, idx + 1) + line.text.slice(idx + iterPrefixLen);
        iterOut.push({ ...line, text: stripped });
      }
      if (nodeMatched) {
        // Fallback view keeps `#<iter>/` so mixed iterations stay legible.
        nodeOut.push(line);
      }
    }
    return iterOut.length > 0 ? iterOut : nodeOut;
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


  const lineCount = filterNodeId ? nodeFiltered.length : annotated.length;
  const droppedBytes = log.start;
  const totalBytes = log.total;
  const isFiltered = filterNodeId != null;

  return (
    <div className="h-full flex flex-col bg-surface-1 min-h-0">
      <div className="px-3 py-1.5 border-b border-border-default flex flex-wrap items-center gap-2 text-micro">
        {showTitle && <span className="font-medium text-fg-muted">Logs</span>}
        <span className="text-fg-subtle">
          {filtered.length} / {lineCount} lines
          {!isFiltered && <> · {formatBytes(totalBytes)}</>}
        </span>
        {!isFiltered && droppedBytes > 0 && prefixState !== "ready" && (
          <span
            className="text-warning-fg text-caption"
            title={`${formatBytes(droppedBytes)} of older output rolled out of the in-memory tail; download the full log to inspect.`}
          >
            +{formatBytes(droppedBytes)} older evicted
          </span>
        )}
        {log.terminated && (
          <span className="text-fg-subtle text-caption italic">stream ended</span>
        )}
        {!log.subscribed && !log.terminated && (
          <span className="text-fg-subtle text-caption italic">connecting…</span>
        )}
        <div className="flex-1 min-w-[140px] max-w-[320px]">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search log…"
            size="sm"
            leadingIcon={<span className="text-caption">⌕</span>}
          />
        </div>
        <Button
          variant="ghost"
          size="sm"
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
          title={
            isFiltered
              ? "Copy the visible (per-node) log lines to the clipboard"
              : "Copy the full run.log to the clipboard"
          }
        >
          copy
        </Button>
        {!isFiltered && (
          <Button
            variant="ghost"
            size="sm"
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
                  downloadBlob(blob, `${runId}.log`);
                } catch (err) {
                  console.error("[LogLinesView] download log failed:", err);
                  useUIStore.getState().addToast("Download failed", "error");
                }
              })();
            }}
            title="Save the full run.log to a file"
          >
            download
          </Button>
        )}
        <Popover
          side="bottom"
          align="end"
          contentClassName="p-1 min-w-[140px]"
          trigger={
            <Button
              variant="secondary"
              size="sm"
              aria-pressed={activeLevels.size > 0}
              title={
                activeLevels.size > 0
                  ? `${activeLevels.size} level filter(s) active`
                  : "Filter by level"
              }
              className={
                activeLevels.size > 0 ? "border-accent" : "text-fg-subtle"
              }
              leadingIcon={<MixerHorizontalIcon className="w-3 h-3" />}
            >
              Levels
              {activeLevels.size > 0 && (
                <span className="font-mono text-[9px] px-1 rounded bg-accent/20 text-accent-fg">
                  {activeLevels.size}
                </span>
              )}
            </Button>
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
                  aria-pressed={isActive}
                  className={`text-micro px-2 py-1 rounded border text-left transition-colors ${
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
              <Button
                variant="ghost"
                size="sm"
                onClick={clearActiveLevels}
                className="mt-1 justify-start"
              >
                Clear filters
              </Button>
            )}
          </div>
        </Popover>
        <label className="ml-auto inline-flex items-center gap-1.5 cursor-pointer">
          <Checkbox
            checked={showSidePanel}
            onChange={(e) => setShowSidePanel(e.target.checked)}
          />
          Tasks
        </label>
        <label className="inline-flex items-center gap-1.5 cursor-pointer">
          <Checkbox
            checked={wordWrap}
            onChange={(e) => setWordWrap(e.target.checked)}
          />
          Wrap
        </label>
        <label className="inline-flex items-center gap-1.5 cursor-pointer">
          <Checkbox
            checked={followTail}
            onChange={(e) => handleToggleFollow(e.target.checked)}
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
            <div className="text-fg-subtle py-2 text-micro">
              {emptyMessage(
                lineCount,
                isFiltered,
                log.subscribed,
                log.total,
                prefixState,
              )}
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
  // scrollbar). Wrap mode: pre-wrap with `break-words` (overflow-wrap)
  // so lines wrap at word boundaries and only genuinely unbreakable
  // long tokens (paths) split — `break-all` was breaking mid-token and
  // dangling tiny fragments ("…main.bot" → "n.bot") on the next row.
  const widthCls = wrap
    ? "whitespace-pre-wrap break-words"
    : "whitespace-pre min-w-max";
  return (
    <div className={`font-mono text-caption py-0.5 ${widthCls} ${cls}`}>
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
    ? "whitespace-pre-wrap break-words"
    : "whitespace-pre min-w-max";
  // Collapsed preview: the first non-empty body line, stripped of the
  // block-indent gutter ("         │ "). Shown inline after the header so
  // the operator reads the gist (e.g. a 💬 message or tool input) without
  // having to expand every block. CSS-truncated to one line with an
  // ellipsis; the full body still appears on click.
  const preview = useMemo(() => {
    for (const l of body) {
      const s = (
        l.text.startsWith(BLOCK_INDENT) ? l.text.slice(BLOCK_INDENT.length) : l.text
      ).trim();
      if (s) return s;
    }
    return "";
  }, [body]);
  const toggle = () => setOpen((v) => !v);
  return (
    <div className={`font-mono text-caption py-0.5 ${cls}`}>
      <div
        {...clickableRowProps(toggle, open ? "Collapse log line" : "Expand log line")}
        aria-expanded={open}
        title={open ? "Collapse body" : "Expand body"}
        className="flex items-baseline gap-2 pr-3 rounded cursor-pointer hover:bg-surface-2"
      >
        {open ? (
          <div className={`flex-1 min-w-0 ${bodyWidthCls}`}>
            {header.text || " "}
          </div>
        ) : (
          // Collapsed: header + dimmed inline preview on a single
          // truncated line. `truncate` needs the flex child to stay
          // shrinkable, hence min-w-0 + no min-w-max here.
          <div className="flex-1 min-w-0 truncate">
            <span>{header.text || " "}</span>
            {preview && <span className="text-fg-subtle"> {preview}</span>}
          </div>
        )}
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

// PrefixState tracks the evicted-head fetch the replay scrubber relies
// on: "none" when no fetch is needed (not clamping, or nothing was
// evicted), then loading → ready | error.
type PrefixState = "none" | "loading" | "ready" | "error";

function emptyMessage(
  lineCount: number,
  isFiltered: boolean,
  subscribed: boolean,
  totalBytes: number,
  prefixState: PrefixState,
): string {
  if (lineCount > 0) return "No log lines match.";
  if (isFiltered) return "No log lines tagged with this node yet.";
  // Replay-scrub states: the log exists but the scrub position falls in
  // the evicted head being (re)fetched. Never claim "No log captured."
  // while totalBytes proves otherwise.
  if (prefixState === "loading") return "Loading earlier log…";
  if (prefixState === "error")
    return "Earlier log unavailable — fetching the evicted portion failed (see console).";
  if (subscribed) return "Waiting for log output…";
  if (totalBytes > 0) return "No log output at this replay position.";
  return "No log captured.";
}
