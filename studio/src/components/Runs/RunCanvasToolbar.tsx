import {
  ArrowDownIcon,
  ArrowRightIcon,
  Crosshair2Icon,
  FrameIcon,
} from "@radix-ui/react-icons";

import { useUIStore } from "@/store/ui";
import { IconButton } from "@/components/ui";
import { LiveDot } from "@/components/ui/LiveDot";

interface Props {
  onFitView: () => void;
  // Recenters the viewport on the currently-running node(s). Disabled
  // when no node is running (typical post-completion state). Distinct
  // from the FollowLivePill in the detail panel, which auto-selects
  // the running execution but leaves the viewport untouched.
  onFocusRunning: () => void;
  runningCount: number;
  // Toggle TB ↔ LR layout. Shares the global UI store with the
  // studio canvas so the user's preference persists across views.
  onToggleLayoutDirection: () => void;
  followLive: boolean;
  onToggleFollowLive: () => void;
}

// Lite toolbar for the run view canvas — mirrors a subset of the
// studio's CanvasToolbar (Fit / layout direction) plus run-only
// Focus-running and Follow-live actions. Kept inline (no popover)
// since the action set is small and discoverability matters more
// than chrome economy here. Positioning is owned by the caller (sits
// next to the status filter chips in RunCanvasIR's top-right strip)
// so this is just a flex row.
export default function RunCanvasToolbar({
  onFitView,
  onFocusRunning,
  runningCount,
  onToggleLayoutDirection,
  followLive,
  onToggleFollowLive,
}: Props) {
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const layoutLabel =
    layoutDirection === "DOWN"
      ? "Switch to horizontal layout (left→right)"
      : "Switch to vertical layout (top→bottom)";
  const focusLabel =
    runningCount === 0
      ? "No node currently running"
      : runningCount === 1
      ? "Center on the running node"
      : `Center on the ${runningCount} running nodes`;
  const followLabel = followLive
    ? "Auto-follow running node is ON — click to pin manually"
    : "Click to auto-follow the running node";
  return (
    <div className="flex items-center gap-1">
      <button
        type="button"
        onClick={onToggleFollowLive}
        aria-pressed={followLive}
        title={followLabel}
        aria-label={followLabel}
        className={`inline-flex items-center gap-1 h-7 px-2 rounded-md border text-[10px] font-medium transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
          followLive
            ? "bg-surface-2 text-fg-default border-border-strong"
            : "bg-surface-1/90 text-fg-subtle border-border-strong hover:text-fg-default hover:bg-surface-2"
        }`}
      >
        {followLive && <LiveDot tone="info" size="xs" />}
        Live
      </button>
      <IconButton
        size="sm"
        variant="secondary"
        label={layoutLabel}
        onClick={onToggleLayoutDirection}
        className="bg-surface-1/90 border-border-strong"
      >
        {layoutDirection === "DOWN" ? <ArrowRightIcon /> : <ArrowDownIcon />}
      </IconButton>
      <IconButton
        size="sm"
        variant="secondary"
        label="Fit view"
        tooltip="Fit all nodes"
        onClick={onFitView}
        className="bg-surface-1/90 border-border-strong"
      >
        <FrameIcon />
      </IconButton>
      <IconButton
        size="sm"
        variant="secondary"
        label={focusLabel}
        tooltip={focusLabel}
        onClick={onFocusRunning}
        disabled={runningCount === 0}
        className="bg-surface-1/90 border-border-strong"
      >
        <Crosshair2Icon />
      </IconButton>
    </div>
  );
}
