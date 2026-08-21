import { useState } from "react";
import type { Node, NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";

import type { ExecutionState, RunSummary } from "@/api/runs";
import type { NodeKind } from "@/api/types";
import { statusDotClass } from "@/lib/subRuns";
import { Popover } from "@/components/ui";
import { ContextUsageBar } from "@/components/ui/ContextUsageBar";
import { EffortBar, isEffortLevel } from "@/components/ui/EffortBar";
import { ProviderIcon } from "@/components/icons/ProviderIcon";
import { BackendBadge } from "@/components/icons/BackendBadge";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { softColor } from "@/lib/constants";

import { displayModel, modelTooltip } from "@/lib/modelLabel";
import { useResolvedModel } from "@/hooks/useResolvedModel";

import { statusClasses, type UnifiedStatus } from "./runStatusClasses";

// Maximum pips to show inline before condensing into a "+N" overflow
// affordance. Tuned to match the 200px node width — beyond ~6 the strip
// either wraps or the node grows taller.
const INLINE_PIPS_MAX = 6;

// Maximum per-child status dots on a subbot card's sub-run chip before
// truncating — same 200px width budget as the pip strip.
const SUBRUN_DOTS_MAX = 8;

// Palette cycled through iteration indices so a loop body that fired
// 5 times shows pip 0 cyan, pip 1 violet, pip 2 amber, etc. Independent
// of status colors — the eye can track "which iteration?" separately
// from "did it succeed?". Backed by `--color-iteration-*` tokens for
// theme-awareness; values resolve at paint time.
export const ITERATION_PALETTE = [
  "var(--color-iteration-0)",
  "var(--color-iteration-1)",
  "var(--color-iteration-2)",
  "var(--color-iteration-3)",
  "var(--color-iteration-4)",
  "var(--color-iteration-5)",
];

export function iterationColor(index: number): string {
  return ITERATION_PALETTE[index % ITERATION_PALETTE.length]!;
}

// LLMMeta is the per-node LLM-call metadata projected onto the run
// canvas. `model`/`backend`/`reasoningEffort` reflect the active value
// (runtime override when present, declared value otherwise). The
// `runtimeOverridden*` flags signal divergence from the declared value
// so the UI can show a "live" badge. `effortIsResolvedDefault` is true
// when `reasoningEffort` was filled from the provider's documented
// default (registry) rather than declared in the workflow or set at
// runtime — used to render the badge in attenuated style.
// `effortSupported` carries the model's supported effort levels so
// EffortBar can normalise to the model's max (a gpt-5 node at "high"
// shows a full bar instead of 3/5).
// `contextWindow`/`contextUsed` drive the ContextUsageBar; both come
// from the backend's effective-model metadata and stay undefined for
// backends/proxies that don't report the window.
export interface LLMMeta {
  model?: string;
  backend?: string;
  reasoningEffort?: string;
  runtimeOverriddenModel?: boolean;
  runtimeOverriddenEffort?: boolean;
  effortIsResolvedDefault?: boolean;
  effortSupported?: string[];
  contextWindow?: number;
  contextUsed?: number;
}

interface IRNodeData extends Record<string, unknown> {
  id: string;
  kind: string;
  // All executions of this IR node, ordered by start time. Empty list
  // = node hasn't been visited yet in this run.
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  // NOT a scalar `loop_iteration` — multiple executions can share that
  // (see comment on RunCanvasIR.defaultIterationFor). Clamped to the
  // valid range when the executions array grows.
  selectedIteration: number;
  isEntry: boolean;
  selected: boolean;
  onSelectIteration: (nodeId: string, iteration: number) => void;
  // Optional LLM metadata for agent/judge/router-llm nodes. Absent for
  // tool/human/compute/router-non-llm/done/fail.
  meta?: LLMMeta;
  // Set for nodes inside a fan_out_each replicated region. `count` = the
  // number of distinct branch items that executed this node; `total` = the
  // items the router fanned out over. count == total → common per-item path
  // (every item passes through, e.g. mark_ip/route); count < total → a
  // routed split (only a subset reached this node, e.g. a per-type stub).
  replication?: { count: number; total: number } | null;
  // Subbot node identity (wire contract C2): the child .bot file this
  // node runs + whether the child executes in an isolated workspace.
  // Present only for kind === "subbot".
  subbot?: { source?: string; isolated?: boolean };
  // Child runs this subbot node spawned — a display-only status-dot row
  // on the COMPACT card (once the child workflow loads, the node
  // expands into a frame whose tabs take over). Absent until the first
  // child run is observed.
  subRuns?: { children: RunSummary[] };
  // Display label override: inline subbot child nodes show their
  // frame-local name instead of the `${subbotId}::${childId}` id.
  label?: string;
}

type IRNodeType = Node<IRNodeData, "irnode">;

export default function IRNode({ data }: NodeProps<IRNodeType>) {
  const { id, kind, executions, selectedIteration, isEntry, selected, onSelectIteration, meta, replication, subbot, subRuns, label: labelOverride } =
    data;
  // Multi-instance badge: count/total when split, ×N when on the common
  // per-item path. Distinguishes "mark_ip ran 38×" (×38) from "code_stub
  // ran 13× because route split the 38 by type" (13/38).
  const repBadge = replication
    ? replication.count === replication.total
      ? `×${replication.total}`
      : `${replication.count}/${replication.total}`
    : null;
  const hasMeta =
    !!meta && (!!meta.model || !!meta.backend || !!meta.reasoningEffort);
  const resolvedModel = useResolvedModel(meta?.model);
  const modelLabel = displayModel(meta?.model, resolvedModel);
  const modelTitle = modelTooltip(meta?.model, resolvedModel);

  // Clamp the index in case the executions array shrunk (run reset).
  const activeIdx = Math.min(Math.max(selectedIteration, 0), executions.length - 1);
  const activeExec = executions[activeIdx] ?? null;
  const selectedStatus: UnifiedStatus = activeExec?.status ?? "none";
  // Card color reflects the DOMINANT status across all attempts: a
  // running attempt anywhere on this node should pop blue/running even
  // when the user scrubbed back to inspect a prior finished attempt
  // (the iteration pip strip still flags the live one via its play
  // glyph). Priority: running > paused_waiting_human > selected exec's
  // status. The pip strip + detail panel still key off the selected
  // attempt, so this only changes the card backdrop.
  let dominantStatus: UnifiedStatus = selectedStatus;
  for (const e of executions) {
    if (e.status === "running") { dominantStatus = "running"; break; }
    if (e.status === "paused_waiting_human" && dominantStatus !== "running") {
      dominantStatus = "paused_waiting_human";
    }
  }
  const c = statusClasses(dominantStatus);

  // Tooltip for nodes the run never reached. Without this the IR canvas
  // shows greyed-out cards with no affordance, leaving the operator to
  // wonder whether the node is broken or simply on a branch that wasn't
  // taken. Surfaced as `title=` so it works on hover everywhere.
  const idleTitle =
    executions.length === 0
      ? "Not yet reached by this run. Routers that didn't pick this destination, conditional edges that evaluated false, or nodes downstream of an unfinished branch all land here."
      : undefined;

  return (
    <div
      className={`relative rounded-xl border px-3 py-2 shadow-sm w-[200px] text-xs transition-colors duration-300 ${c.bg} ${c.border} ${c.text} ${
        selected ? "ir-node-selected" : ""
      }`}
      title={idleTitle}
      // Inner ring tinted by the selected iteration index so the user
      // can scan the canvas and see "iteration 3 → all the violet
      // borders". Subtle so it doesn't fight the status color.
      style={
        executions.length > 0
          ? { boxShadow: `inset 0 0 0 2px ${softColor(iterationColor(activeIdx), 20)}` }
          : undefined
      }
    >
      <Handle type="target" position={Position.Top} className="!bg-fg-subtle" />
      <div className="flex items-center gap-1.5 font-medium truncate">
        <NodeIcon kind={kind as NodeKind} size={14} className="shrink-0" />
        <span className="truncate" title={id}>
          {labelOverride ?? id}
        </span>
        {isEntry && (
          <span
            className="ml-auto text-[9px] uppercase bg-warning-soft text-warning-fg px-1 rounded"
            title="entry node"
          >
            entry
          </span>
        )}
      </div>
      <div className="mt-1 flex items-center gap-1.5 text-caption text-fg-subtle">
        <span>{kind}</span>
        {repBadge && (
          <span
            className="px-1 rounded bg-info-soft text-info-fg text-[9px] font-mono leading-tight"
            title={
              replication!.count === replication!.total
                ? `Runs once per item — ${replication!.total} executions (common per-item path).`
                : `${replication!.count} of ${replication!.total} items routed here (subset of the fan-out).`
            }
          >
            ‖ {repBadge}
          </span>
        )}
        <span className="ml-auto">
          {executions.length === 0
            ? "—"
            : executions.length === 1
            ? selectedStatus
            : `${selectedStatus} · iter ${activeIdx + 1}/${executions.length}`}
        </span>
      </div>

      {/* Subbot identity: which .bot file the node runs (mono, like a
          command line) + an isolated-workspace mini-chip. */}
      {kind === "subbot" && subbot?.source && (
        <div className="mt-1 flex items-center gap-1 text-caption text-fg-subtle min-w-0">
          <span className="font-mono truncate" title={subbot.source}>
            {subbot.source}
          </span>
          {subbot.isolated && (
            <span
              className="px-1 rounded border border-border-default bg-surface-2 text-fg-muted text-[8px] uppercase shrink-0"
              title="isolated: the child run executes in its own workspace"
            >
              isolated
            </span>
          )}
        </div>
      )}

      {hasMeta && (
        <>
          {modelLabel && (
            <div className="mt-1 flex items-center gap-1 text-caption text-fg-subtle min-w-0">
              <ProviderIcon
                model={resolvedModel || meta?.model}
                delegate={meta?.backend}
                size={10}
                className="shrink-0 opacity-70"
              />
              <span className="truncate" title={modelTitle ?? meta?.model}>
                {modelLabel}
              </span>
              {meta?.runtimeOverriddenModel && (
                <span
                  className="ml-1 px-1 rounded bg-info-soft text-info-fg text-[8px] uppercase shrink-0"
                  title="model overridden at runtime"
                >
                  live
                </span>
              )}
            </div>
          )}
          <ContextUsageBar
            used={meta?.contextUsed}
            window={meta?.contextWindow}
          />
          <div className="mt-0.5 flex items-center gap-1.5 text-caption text-fg-subtle flex-wrap">
            <BackendBadge backend={meta?.backend} size={10} />
            {isEffortLevel(meta?.reasoningEffort) && (
              <EffortBar
                level={meta.reasoningEffort}
                live={meta.runtimeOverriddenEffort}
                muted={meta.effortIsResolvedDefault}
                supported={meta.effortSupported}
              />
            )}
          </div>
        </>
      )}

      {/* Iteration timeline: one pip per execution. Pip color =
          iteration palette (index-based). Filled when status is
          terminal; pulse when running. Selected pip has thicker
          border + scale. Click to switch. */}
      {executions.length > 1 && (
        <IterationTimeline
          nodeId={id}
          executions={executions}
          selectedIteration={selectedIteration}
          onSelectIteration={onSelectIteration}
        />
      )}

      {subRuns && subRuns.children.length > 0 && (
        <SubRunChip children_={subRuns.children} />
      )}

      <Handle type="source" position={Position.Bottom} className="!bg-fg-subtle" />
    </div>
  );
}

// SubRunChip is the compact "N sub-runs" status row on a COMPACT subbot
// card: one status dot per child (capped to fit the 200px width budget)
// + count label. Display-only — the transient pre-expansion state; once
// the child workflow loads the node becomes a frame with its own tabs.
function SubRunChip({ children_ }: { children_: RunSummary[] }) {
  const shown = children_.slice(0, SUBRUN_DOTS_MAX);
  const label = `${children_.length} sub-run${children_.length === 1 ? "" : "s"}`;
  return (
    <div
      className="mt-1.5 flex items-center gap-1 text-[9px] font-mono rounded border border-border-default bg-surface-1 px-1.5 h-[16px] text-fg-muted max-w-full"
      title={label}
    >
      {shown.map((c) => (
        <span
          key={c.id}
          className={`inline-block h-1.5 w-1.5 rounded-full shrink-0 ${statusDotClass(c.status)}`}
          aria-hidden
        />
      ))}
      <span className="truncate">{label}</span>
    </div>
  );
}

function IterationTimeline({
  nodeId,
  executions,
  selectedIteration,
  onSelectIteration,
}: {
  nodeId: string;
  // Ordered by start time — RunCanvasIR groups + the snapshot reducer
  // preserves start order. Indexing into this array is the per-attempt
  // identifier the UI uses (scalar loop_iteration is not unique under
  // Option 3 nested-loop exec_ids).
  executions: ExecutionState[];
  // 0-based index into `executions` of the currently selected attempt.
  selectedIteration: number;
  // Callback receives the selected attempt's array index.
  onSelectIteration: (nodeId: string, index: number) => void;
}) {
  const overflow = executions.length > INLINE_PIPS_MAX;
  // When overflowing, show INLINE_PIPS_MAX-1 most-recent pips + the
  // "+N" affordance. If the selected attempt falls outside that
  // window, also pin it inline so the user always sees what they
  // currently have selected.
  let inline: Array<{ exec: ExecutionState; index: number }>;
  let extra: Array<{ exec: ExecutionState; index: number }>;
  const indexed = executions.map((exec, index) => ({ exec, index }));
  if (!overflow) {
    inline = indexed;
    extra = [];
  } else {
    const tail = indexed.slice(-(INLINE_PIPS_MAX - 1));
    const tailSet = new Set(tail.map((e) => e.index));
    if (selectedIteration >= 0 && !tailSet.has(selectedIteration)) {
      // Insert the selected pip at the front, dropping the oldest
      // tail entry to keep the strip width bounded.
      const sel = indexed[selectedIteration];
      if (sel) {
        inline = [sel, ...tail.slice(1)];
      } else {
        inline = tail;
      }
    } else {
      inline = tail;
    }
    const inlineIdxSet = new Set(inline.map((e) => e.index));
    extra = indexed.filter((e) => !inlineIdxSet.has(e.index));
  }

  return (
    <div
      className="mt-1.5 flex items-center gap-1 flex-wrap"
      onClick={(e) => {
        // ReactFlow's node-click handler would otherwise fire when the
        // user clicks inside the pip strip.
        e.stopPropagation();
      }}
    >
      {inline.map(({ exec, index }) => (
        <IterationPip
          key={exec.execution_id}
          exec={exec}
          index={index}
          selected={index === selectedIteration}
          onClick={() => onSelectIteration(nodeId, index)}
        />
      ))}
      {extra.length > 0 && (
        <OverflowPopover
          label={`+${extra.length}`}
          executions={extra}
          selectedIteration={selectedIteration}
          onSelectIteration={(idx) => onSelectIteration(nodeId, idx)}
        />
      )}
    </div>
  );
}

function IterationPip({
  exec,
  index,
  selected,
  onClick,
}: {
  exec: ExecutionState;
  index: number;
  selected: boolean;
  onClick: () => void;
}) {
  const palette = iterationColor(index);
  const statusGlyph = statusClasses(exec.status).glyph;
  return (
    <button
      type="button"
      onClick={onClick}
      className={`flex items-center justify-center text-[9px] font-mono rounded ${
        selected
          ? "ring-1 ring-fg-default scale-110"
          : "opacity-70 hover:opacity-100"
      }`}
      style={{
        backgroundColor: softColor(palette, 20),
        border: `1px solid ${palette}`,
        color: palette,
        minWidth: 18,
        height: 16,
        padding: "0 3px",
      }}
      title={`Iteration ${index + 1} · ${exec.status}`}
    >
      {index + 1}
      <span className="ml-0.5">{statusGlyph}</span>
    </button>
  );
}

function OverflowPopover({
  label,
  executions,
  selectedIteration,
  onSelectIteration,
}: {
  label: string;
  // Pre-indexed so we can pass the array position straight through.
  executions: Array<{ exec: ExecutionState; index: number }>;
  selectedIteration: number;
  onSelectIteration: (index: number) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      trigger={
        <button
          type="button"
          className="flex items-center justify-center text-[9px] font-mono rounded bg-surface-2 border border-border-default text-fg-muted hover:text-fg-default hover:bg-surface-3"
          style={{
            minWidth: 22,
            height: 16,
            padding: "0 3px",
          }}
          title={`${executions.length} more iterations`}
        >
          {label}
        </button>
      }
    >
      <div className="grid grid-cols-4 gap-1 p-2 max-w-[180px]">
        {executions.map(({ exec, index }) => (
          <IterationPip
            key={exec.execution_id}
            exec={exec}
            index={index}
            selected={index === selectedIteration}
            onClick={() => {
              onSelectIteration(index);
              setOpen(false);
            }}
          />
        ))}
      </div>
    </Popover>
  );
}
