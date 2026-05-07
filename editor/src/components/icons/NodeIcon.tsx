import type { NodeKind } from "@/api/types";
import { NODE_LUCIDE_ICONS, NODE_COLORS } from "@/lib/constants";

interface Props {
  kind: NodeKind;
  size?: number;
  className?: string;
}

export function NodeIcon({ kind, size = 16, className }: Props) {
  const Icon = NODE_LUCIDE_ICONS[kind];
  if (!Icon) return null;
  return (
    <Icon
      size={size}
      strokeWidth={1.75}
      style={{ color: NODE_COLORS[kind] }}
      className={className}
      aria-hidden
    />
  );
}
