import { Cross2Icon } from "@radix-ui/react-icons";
import type { HTMLAttributes, ReactNode } from "react";

export interface ChipProps extends HTMLAttributes<HTMLSpanElement> {
  onRemove?: () => void;
  removeLabel?: string;
  leadingIcon?: ReactNode;
}

export function Chip({
  onRemove,
  removeLabel = "Remove",
  leadingIcon,
  className = "",
  children,
  ...rest
}: ChipProps) {
  const base =
    "inline-flex items-center gap-1 rounded-md border border-border-default bg-surface-2 text-fg-default text-xs h-6 px-2 max-w-full";
  return (
    <span className={`${base} ${className}`.trim()} {...rest}>
      {leadingIcon}
      <span className="truncate">{children}</span>
      {onRemove && (
        <button
          type="button"
          aria-label={removeLabel}
          onClick={(e) => {
            e.stopPropagation();
            onRemove();
          }}
          className="-mr-1 inline-flex h-4 w-4 items-center justify-center rounded text-fg-subtle hover:bg-surface-3 hover:text-fg-default"
        >
          <Cross2Icon width={10} height={10} />
        </button>
      )}
    </span>
  );
}
