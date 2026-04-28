import * as RT from "@radix-ui/react-tooltip";
import type { ReactNode } from "react";

export interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  delayDuration?: number;
  disabled?: boolean;
}

export function Tooltip({
  content,
  children,
  side = "top",
  align = "center",
  delayDuration = 250,
  disabled = false,
}: TooltipProps) {
  if (disabled || content === null || content === undefined || content === "") {
    return <>{children}</>;
  }
  return (
    <RT.Provider delayDuration={delayDuration}>
      <RT.Root>
        <RT.Trigger asChild>{children}</RT.Trigger>
        <RT.Portal>
          <RT.Content
            side={side}
            align={align}
            sideOffset={6}
            className="z-50 max-w-xs rounded-md border border-border-default bg-surface-2 px-2 py-1 text-xs text-fg-default shadow-lg animate-fade-in"
          >
            {content}
            <RT.Arrow className="fill-[color:var(--color-surface-2)]" />
          </RT.Content>
        </RT.Portal>
      </RT.Root>
    </RT.Provider>
  );
}
