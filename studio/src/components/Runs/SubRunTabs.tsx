import type { KeyboardEvent } from "react";

import type { RunStatus, RunSummary } from "@/api/runs";
import { childStatusTone, childTabLabel } from "@/lib/subRuns";

import { labelForStatus } from "./runStatusMeta";

// Solid-fill dot classes per ChildStatusTone.variant. Full-strength
// status colors (not the -soft tints) — at 6px a tint is invisible.
const DOT_CLASS: Record<ReturnType<typeof childStatusTone>["variant"], string> = {
  info: "bg-info",
  warning: "bg-warning",
  success: "bg-success",
  danger: "bg-danger",
  neutral: "bg-fg-subtle",
};

// statusDotClass returns the classes for a small sub-run status dot —
// shared by the tab strip here and IRNode's per-subbot chip row so the
// two surfaces stay color-identical.
export function statusDotClass(status: RunStatus): string {
  const tone = childStatusTone(status);
  return `${DOT_CLASS[tone.variant]}${tone.pulse ? " animate-pulse" : ""}`;
}

export const MAIN_FLOW_TAB = "main";

interface Props {
  // Label for the parent ("Main") tab — the run's friendly name.
  mainLabel: string;
  // Child runs in created_at order (the /children endpoint's order).
  childRuns: RunSummary[];
  // Reverse index childRunId → spawning subbot node id, built by
  // RunView from groupChildrenByNode. Entries absent for unattributed
  // children (tooltip just omits the spawned-by line).
  nodeIdByChildId: Map<string, string>;
  // MAIN_FLOW_TAB or a child run id.
  active: string;
  onSelect: (id: string) => void;
}

// SubRunTabs is the compact strip above the run canvas that switches
// the flow view between the parent run ("Main") and each subbot child
// run. Purely presentational — RunView owns the active-tab state and
// the mount-on-first-visit content behind it.
export default function SubRunTabs({
  mainLabel,
  childRuns,
  nodeIdByChildId,
  active,
  onSelect,
}: Props) {
  // Roving-tabindex keyboard nav (WAI-ARIA tab pattern, same as
  // InnerTabBar): only the active tab sits in the Tab order;
  // Left/Right/Home/End move between tabs and follow focus.
  const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) return;
    const tabButtons = Array.from(
      e.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]'),
    );
    if (tabButtons.length === 0) return;
    const current = tabButtons.findIndex((b) => b === document.activeElement);
    let next: number;
    if (e.key === "Home") next = 0;
    else if (e.key === "End") next = tabButtons.length - 1;
    else if (e.key === "ArrowRight")
      next = current < 0 ? 0 : (current + 1) % tabButtons.length;
    else next = current <= 0 ? tabButtons.length - 1 : current - 1;
    e.preventDefault();
    const btn = tabButtons[next];
    btn?.focus();
    const id = btn?.getAttribute("data-tab-id");
    if (id) onSelect(id);
  };

  return (
    <div
      role="tablist"
      aria-label="Run flow"
      onKeyDown={handleKeyDown}
      className="shrink-0 flex items-center gap-1 px-2 py-1 border-b border-border-default bg-surface-1 overflow-x-auto"
    >
      <TabButton
        id={MAIN_FLOW_TAB}
        active={active === MAIN_FLOW_TAB}
        onClick={() => onSelect(MAIN_FLOW_TAB)}
        title={`Parent run · ${mainLabel}`}
      >
        <span className="font-medium">Main</span>
        <span className="text-fg-subtle truncate max-w-[16ch]">{mainLabel}</span>
      </TabButton>
      {childRuns.map((child, i) => {
        const spawnedBy = nodeIdByChildId.get(child.id);
        const title = [
          `Sub-run ${child.id}`,
          labelForStatus(child.status),
          spawnedBy ? `spawned by ${spawnedBy}` : null,
        ]
          .filter(Boolean)
          .join(" · ");
        return (
          <TabButton
            key={child.id}
            id={child.id}
            active={active === child.id}
            onClick={() => onSelect(child.id)}
            title={title}
          >
            <span
              className={`inline-block h-1.5 w-1.5 rounded-full shrink-0 ${statusDotClass(child.status)}`}
              aria-hidden
            />
            <span className="truncate max-w-[18ch]">
              {childTabLabel(child, i)}
            </span>
          </TabButton>
        );
      })}
    </div>
  );
}

function TabButton({
  id,
  active,
  onClick,
  title,
  children,
}: {
  // Tab identity (MAIN_FLOW_TAB or a child run id) — read back by the
  // tablist's keyboard handler via data-tab-id.
  id: string;
  active: boolean;
  onClick: () => void;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      tabIndex={active ? 0 : -1}
      data-tab-id={id}
      onClick={onClick}
      title={title}
      className={`flex items-center gap-1.5 text-caption px-2 py-0.5 rounded border transition-colors whitespace-nowrap ${
        active
          ? "bg-surface-2 border-border-strong text-fg-default"
          : "bg-surface-1 border-border-default text-fg-muted hover:bg-surface-2 hover:text-fg-default"
      }`}
    >
      {children}
    </button>
  );
}
