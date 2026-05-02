import { Badge, type BadgeSize } from "./Badge";
import {
  statusClasses,
  type UnifiedStatus,
} from "@/components/Runs/runStatusClasses";

interface Props {
  status: UnifiedStatus;
  size?: BadgeSize;
  // When true, hide the glyph (callers that already display an icon
  // elsewhere — e.g. iteration pips — pass false).
  showGlyph?: boolean;
  // Override the default human label (e.g. "Failed (resumable)" → "Failed").
  label?: string;
  className?: string;
  title?: string;
}

export function StatusBadge({
  status,
  size = "sm",
  showGlyph = true,
  label,
  className,
  title,
}: Props) {
  const cls = statusClasses(status);
  return (
    <Badge
      variant={cls.badgeVariant}
      size={size}
      className={className}
      title={title ?? cls.label}
    >
      {showGlyph && (
        <span aria-hidden className="leading-none">
          {cls.glyph}
        </span>
      )}
      <span>{label ?? cls.label}</span>
    </Badge>
  );
}
