import { LiveDot } from "@/components/ui";

// FollowLivePill toggles the parent's auto-tracking of the running
// node. When active, the panel jumps to whatever the engine is
// currently working on; when off, the user's manual selection stays
// pinned. The visual is a pill with a pulsing dot when active so it
// reads as "live" at a glance.
export function FollowLivePill({
  followLive,
  onToggle,
}: {
  followLive: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-pressed={followLive}
      className={`inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-caption border transition-colors ${
        followLive
          ? "bg-success-soft border-success text-success-fg"
          : "bg-surface-1 border-border-default text-fg-subtle hover:text-fg-default"
      }`}
      title={
        followLive
          ? "Auto-following the running node. Click to pin on the current selection."
          : "Pinned. Click to follow the running node."
      }
    >
      <LiveDot tone={followLive ? "success" : "neutral"} size="sm" pulse={followLive} />
      live
    </button>
  );
}
