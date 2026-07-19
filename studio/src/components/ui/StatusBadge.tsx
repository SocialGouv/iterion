import { Badge, type BadgeSize, type BadgeVariant } from "./Badge";
import {
  statusClasses,
  type UnifiedStatus,
} from "@/components/Runs/runStatusClasses";

interface BaseProps {
  size?: BadgeSize;
  // When true, hide the glyph (callers that already display an icon
  // elsewhere — e.g. iteration pips — pass false).
  showGlyph?: boolean;
  className?: string;
  title?: string;
}

interface RunStatusProps extends BaseProps {
  status: UnifiedStatus;
  variant?: never;
  // Override the default human label (e.g. "Failed (resumable)" → "Failed").
  label?: string;
}

// Custom mode for callers whose status vocabulary is not a run/exec
// status (queued-message lifecycle, HTTP delivery status): supply the
// Badge variant and label directly. No glyph — glyphs belong to the
// unified run-status taxonomy.
interface CustomProps extends BaseProps {
  status?: never;
  variant: BadgeVariant;
  label: string;
}

type Props = RunStatusProps | CustomProps;

export function StatusBadge({
  status,
  variant,
  size = "sm",
  showGlyph = true,
  label,
  className,
  title,
}: Props) {
  const cls = status !== undefined ? statusClasses(status) : null;
  const text = label ?? cls?.label ?? "";
  return (
    <Badge
      variant={variant ?? cls?.badgeVariant ?? "neutral"}
      size={size}
      className={className}
      title={title ?? cls?.label ?? text}
    >
      {showGlyph && cls && (
        <span aria-hidden className="leading-none">
          {cls.glyph}
        </span>
      )}
      <span>{text}</span>
    </Badge>
  );
}
