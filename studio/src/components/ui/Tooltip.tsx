import * as RT from "@radix-ui/react-tooltip";
import { useState, type ReactNode } from "react";
import { useHiddenSubtree } from "./hiddenSubtree";

export interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  delayDuration?: number;
  disabled?: boolean;
  /** Keeps the trigger at one place in the tree whether or not there is
   *  anything to show. Without it a trigger that gains or loses its tooltip
   *  is remounted — and a control that had the keyboard focus loses it. */
  stable?: boolean;
}

export function Tooltip({
  content,
  children,
  side = "top",
  align = "center",
  delayDuration = 250,
  disabled = false,
  stable = false,
}: TooltipProps) {
  const hidden = useHiddenSubtree();
  // A stable tooltip is controlled: with nothing to show it stays closed,
  // rather than leave the tree.
  const [open, setOpen] = useState(false);
  const empty = disabled || content === null || content === undefined || content === "";
  // Nothing left to show closes it for good: left "open" under an empty
  // content, it would show again the moment content comes back — with the
  // focus long gone elsewhere.
  if (stable && empty && open) setOpen(false);
  if (empty && !stable) {
    return <>{children}</>;
  }
  return (
    <RT.Provider delayDuration={delayDuration}>
      <RT.Root {...(stable ? { open: !empty && open, onOpenChange: setOpen } : {})}>
        <RT.Trigger asChild>{children}</RT.Trigger>
        {!empty && !hidden && (
        <RT.Portal>
          <RT.Content
            side={side}
            align={align}
            sideOffset={6}
            className="z-[var(--z-tooltip)] max-w-xs rounded-md border border-border-default bg-surface-2 px-2 py-1 text-xs text-fg-default shadow-[var(--shadow-popover)] animate-fade-in"
          >
            {content}
            <RT.Arrow className="fill-[color:var(--color-surface-2)]" />
          </RT.Content>
        </RT.Portal>
        )}
      </RT.Root>
    </RT.Provider>
  );
}
