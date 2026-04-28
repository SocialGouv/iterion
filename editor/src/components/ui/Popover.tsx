import * as RP from "@radix-ui/react-popover";
import type { ReactNode } from "react";

export interface PopoverProps {
  trigger: ReactNode;
  children: ReactNode;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  contentClassName?: string;
  modal?: boolean;
}

export function Popover({
  trigger,
  children,
  side = "bottom",
  align = "start",
  open,
  onOpenChange,
  contentClassName = "",
  modal = false,
}: PopoverProps) {
  return (
    <RP.Root open={open} onOpenChange={onOpenChange} modal={modal}>
      <RP.Trigger asChild>{trigger}</RP.Trigger>
      <RP.Portal>
        <RP.Content
          side={side}
          align={align}
          sideOffset={6}
          className={`z-50 rounded-md border border-border-default bg-surface-1 text-fg-default shadow-xl animate-fade-in ${contentClassName}`.trim()}
        >
          {children}
        </RP.Content>
      </RP.Portal>
    </RP.Root>
  );
}

export const PopoverClose = RP.Close;
