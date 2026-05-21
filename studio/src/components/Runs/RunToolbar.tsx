import { InfoCircledIcon, ListBulletIcon } from "@radix-ui/react-icons";

interface Props {
  detailCollapsed: boolean;
  onToggleDetail: () => void;
  eventlogCollapsed: boolean;
  onToggleEventlog: () => void;
}

// Files toggle is intentionally omitted — LeftPanel exposes its own
// activity-bar affordance when collapsed. Chat toggle is the floating
// bubble in the bottom-right (FloatingChatPanel).
export default function RunToolbar({
  detailCollapsed,
  onToggleDetail,
  eventlogCollapsed,
  onToggleEventlog,
}: Props) {
  return (
    <div className="shrink-0 border-b border-border-default px-3 py-1 flex items-center gap-1 bg-surface-0">
      <ToolbarButton
        active={!detailCollapsed}
        onClick={onToggleDetail}
        icon={<InfoCircledIcon className="h-3.5 w-3.5" />}
        label="Node detail"
        title="Show / hide the node detail panel"
      />
      <ToolbarButton
        active={!eventlogCollapsed}
        onClick={onToggleEventlog}
        icon={<ListBulletIcon className="h-3.5 w-3.5" />}
        label="Events / logs"
        title="Show / hide the bottom drawer (events, logs, report, artifacts)"
      />
    </div>
  );
}

function ToolbarButton({
  active,
  onClick,
  icon,
  label,
  title,
}: {
  active: boolean;
  onClick: () => void;
  icon: React.ReactNode;
  label: string;
  title: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      aria-pressed={active}
      className={`inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-[11px] font-medium transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent-fg ${
        active
          ? "bg-surface-2 text-fg-default"
          : "text-fg-subtle hover:text-fg-default hover:bg-surface-1"
      }`}
    >
      {icon}
      <span>{label}</span>
    </button>
  );
}
