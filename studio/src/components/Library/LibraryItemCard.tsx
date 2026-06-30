import type { DragEvent } from "react";
import type { LibraryItem, LibraryCategory } from "@/lib/library/types";
import { NODE_ICONS, NODE_COLORS } from "@/lib/constants";
import { clickableRowProps } from "@/lib/a11y";
import { LAYER_COLORS, LAYER_ICONS } from "@/lib/constants";

function getCategoryColor(category: LibraryCategory): string {
  if (category === "pattern") return "var(--color-library-pattern)";
  if (category in NODE_COLORS) return NODE_COLORS[category as keyof typeof NODE_COLORS];
  if (category === "schema") return LAYER_COLORS.schemas;
  if (category === "prompt") return LAYER_COLORS.prompts;
  if (category === "var") return LAYER_COLORS.vars;
  return "var(--color-fg-subtle)";
}

function getCategoryIcon(category: LibraryCategory): string {
  if (category === "pattern") return "\u{1F9E9}";
  if (category in NODE_ICONS) return NODE_ICONS[category as keyof typeof NODE_ICONS];
  if (category === "schema") return LAYER_ICONS.schemas;
  if (category === "prompt") return LAYER_ICONS.prompts;
  if (category === "var") return LAYER_ICONS.vars;
  return "?";
}

interface Props {
  item: LibraryItem;
  onAdd: (item: LibraryItem) => void;
}

export default function LibraryItemCard({ item, onAdd }: Props) {
  const color = getCategoryColor(item.category);
  const icon = getCategoryIcon(item.category);

  const onDragStart = (e: DragEvent) => {
    e.dataTransfer.setData("application/iterion-library", item.id);
    e.dataTransfer.effectAllowed = "move";
  };

  return (
    <div
      draggable
      onDragStart={onDragStart}
      {...clickableRowProps(() => onAdd(item), `Add ${item.name} to canvas`)}
      className="flex items-start gap-2 px-2 py-2 rounded cursor-grab hover:bg-surface-2/50 transition-colors border-l-2 group focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent"
      style={{ borderLeftColor: color }}
      title={item.description}
    >
      <span className="text-sm mt-0.5 shrink-0">{icon}</span>
      <div className="min-w-0 flex-1">
        <div className="text-xs font-medium text-fg-default flex items-center gap-1 min-w-0">
          <span className="truncate">{item.name}</span>
          {item.template.pattern && (
            <span className="text-caption bg-info/20 text-info-fg px-1 rounded shrink-0">
              {item.template.pattern.nodes.length} nodes
            </span>
          )}
        </div>
        <div className="text-caption text-fg-subtle line-clamp-2 leading-tight mt-0.5">{item.description}</div>
        {item.tags && item.tags.length > 0 && (
          <div className="flex gap-1 flex-wrap mt-1">
            {item.tags.slice(0, 3).map((tag) => (
              <span
                key={tag}
                className="text-caption px-1 py-0 rounded bg-surface-2 text-fg-subtle"
              >
                {tag}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
