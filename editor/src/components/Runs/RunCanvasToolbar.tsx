import {
  ArrowDownIcon,
  ArrowRightIcon,
  Crosshair2Icon,
  FrameIcon,
} from "@radix-ui/react-icons";

import { useUIStore } from "@/store/ui";
import { IconButton } from "@/components/ui";

interface Props {
  onFitView: () => void;
  // Recenters the viewport on the currently-running node(s). Disabled
  // when no node is running (typical post-completion state). Distinct
  // from the FollowLivePill in the detail panel, which auto-selects
  // the running execution but leaves the viewport untouched.
  onFocusRunning: () => void;
  runningCount: number;
  // Toggle TB ↔ LR layout. Shares the global UI store with the
  // editor canvas so the user's preference persists across views.
  onToggleLayoutDirection: () => void;
}

// Lite toolbar for the run view canvas — mirrors a subset of the
// editor's CanvasToolbar (Fit / layout direction) plus a run-only
// Focus-running action. Kept inline (no popover) since the action set
// is small and discoverability matters more than chrome economy here.
// Positioning is owned by the caller (sits next to the status filter
// chips in RunCanvasIR's top-right strip) so this is just a flex row.
export default function RunCanvasToolbar({
  onFitView,
  onFocusRunning,
  runningCount,
  onToggleLayoutDirection,
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
  return (
    <div className="flex items-center gap-1">
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
