import type { KeyboardEvent } from "react";
import type { Node, NodeProps } from "@xyflow/react";
import { Handle, Position } from "@xyflow/react";
import { ExternalLink } from "lucide-react";
import { Link } from "wouter";

import type { RunSummary } from "@/api/runs";
import { NodeIcon } from "@/components/icons/NodeIcon";
import { softColor } from "@/lib/constants";
import { childTabLabel, statusDotClass } from "@/lib/subRuns";

import { labelForStatus } from "./runStatusMeta";

// Container node framing an expanded subbot's child PIPELINE on the RUN
// canvas — the runtime twin of the editor's SubbotFrameNode. A real
// React Flow parent (child nodes carry parentId here; ELK sizes it as a
// compound), solid violet border so it reads as "another bot's graph,
// executing as separate child runs".
//
// The header carries one TAB per child run: selecting a tab swaps which
// child's live execution state the pipeline inside displays — the user
// never leaves the parent run's canvas. The external-link icon is the
// only way out (the active child's full console), deliberately small.

const SUBBOT_COLOR = "var(--color-node-subbot)";

// Header band = title row (~34px) + tab row (~28px). autoLayout reads
// this via data.headerHeight to pad the compound's content below it.
export const SUBBOT_RUN_FRAME_HEADER = 64;

export interface SubbotRunFrameData extends Record<string, unknown> {
  label: string;
  source: string;
  isolated: boolean;
  headerHeight: number;
  subRuns?: {
    children: RunSummary[];
    // Child run whose execution state the frame currently displays.
    selectedChildId: string | null;
    onSelectChild?: (frameId: string, childRunId: string) => void;
  };
}

type SubbotRunFrameType = Node<SubbotRunFrameData, "subbotFrame">;

export default function SubbotRunFrame({ id, data }: NodeProps<SubbotRunFrameType>) {
  const { label, source, isolated, subRuns } = data;
  const children = subRuns?.children ?? [];
  const selectedChildId = subRuns?.selectedChildId ?? null;

  // Roving-tabindex keyboard nav (WAI-ARIA tab pattern, same as
  // InnerTabBar): Left/Right/Home/End move between tabs, following
  // focus with selection.
  const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) return;
    const tabs = Array.from(
      e.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]'),
    );
    if (tabs.length === 0) return;
    const current = tabs.findIndex((b) => b === document.activeElement);
    let next: number;
    if (e.key === "Home") next = 0;
    else if (e.key === "End") next = tabs.length - 1;
    else if (e.key === "ArrowRight")
      next = current < 0 ? 0 : (current + 1) % tabs.length;
    else next = current <= 0 ? tabs.length - 1 : current - 1;
    e.preventDefault();
    e.stopPropagation();
    const btn = tabs[next];
    btn?.focus();
    const childId = btn?.getAttribute("data-child-id");
    if (childId) subRuns?.onSelectChild?.(id, childId);
  };

  return (
    <div
      className="rounded-xl border-2 w-full h-full"
      style={{
        borderColor: softColor(SUBBOT_COLOR, 55),
        borderStyle: "solid",
        background: softColor(SUBBOT_COLOR, 5),
        minWidth: "100%",
        minHeight: "100%",
      }}
    >
      {/* Fallback attachment points for the rare parent edge that stays
          on the frame (child workflow without a done terminal). */}
      <Handle type="target" position={Position.Top} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
      <div
        className="flex items-center gap-1.5 px-3 pt-2 pb-1 select-none"
        title={`Subbot ${label}: runs ${source} as ${children.length || "separate"} child run(s).`}
      >
        <NodeIcon kind="subbot" size={14} />
        <span className="text-xs font-semibold text-fg-default">{label}</span>
        <span className="text-[9px] uppercase tracking-wider text-fg-subtle">· subbot</span>
        <span className="font-mono text-caption text-fg-muted truncate">{source}</span>
        {isolated && (
          <span
            className="text-[9px] px-1 rounded"
            style={{ background: softColor(SUBBOT_COLOR, 18), color: SUBBOT_COLOR }}
            title="isolated: each child run confines its writes to its own run store"
          >
            isolated
          </span>
        )}
        {selectedChildId && (
          <Link
            href={`/runs/${encodeURIComponent(selectedChildId)}`}
            className="ml-auto shrink-0 inline-flex items-center justify-center w-5 h-5 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-2"
            title="Open this sub-run's full console"
            aria-label="Open this sub-run's full console"
            onClick={(e) => e.stopPropagation()}
          >
            <ExternalLink size={12} aria-hidden />
          </Link>
        )}
      </div>
      {children.length > 0 && (
        <div
          role="tablist"
          aria-label={`Sub-runs of ${label}`}
          onKeyDown={handleKeyDown}
          className="flex items-center gap-1 px-3 pb-1.5 overflow-x-auto"
          style={{ borderBottom: `1px solid ${softColor(SUBBOT_COLOR, 25)}` }}
        >
          {children.map((child, i) => {
            const active = child.id === selectedChildId;
            return (
              <button
                key={child.id}
                type="button"
                role="tab"
                aria-selected={active}
                tabIndex={active ? 0 : -1}
                data-child-id={child.id}
                title={`${childTabLabel(child, i)} · ${labelForStatus(child.status)} · ${child.id}`}
                className={`flex items-center gap-1.5 text-caption px-2 py-0.5 rounded border transition-colors whitespace-nowrap ${
                  active
                    ? "bg-surface-2 border-border-strong text-fg-default"
                    : "bg-surface-1 border-border-default text-fg-muted hover:bg-surface-2 hover:text-fg-default"
                }`}
                onClick={(e) => {
                  e.stopPropagation();
                  subRuns?.onSelectChild?.(id, child.id);
                }}
              >
                <span
                  className={`inline-block h-1.5 w-1.5 rounded-full shrink-0 ${statusDotClass(child.status)}`}
                  aria-hidden
                />
                <span className="truncate max-w-[16ch]">{childTabLabel(child, i)}</span>
              </button>
            );
          })}
        </div>
      )}
      <Handle type="source" position={Position.Bottom} className="!bg-surface-3 !w-1.5 !h-1.5 !opacity-0" />
    </div>
  );
}
