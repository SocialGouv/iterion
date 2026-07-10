import * as RD from "@radix-ui/react-dialog";
import { Cross2Icon } from "@radix-ui/react-icons";
import type { ReactNode } from "react";

// Right-side sheet built on Radix Dialog — focus trap, Escape,
// click-outside and aria wiring come from Radix. For centered
// modals use ui/Dialog instead.

export interface DrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title?: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  /** Tailwind max-width class for the sheet. */
  widthClass?: string;
}

export function Drawer({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  widthClass = "max-w-xl",
}: DrawerProps) {
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-[var(--z-overlay)] bg-scrim-modal animate-fade-in-opacity" />
        <RD.Content
          className={`fixed inset-y-0 right-0 z-[var(--z-modal)] flex w-full ${widthClass} flex-col border-l border-border-default bg-surface-1 text-fg-default shadow-[var(--shadow-lg)] animate-slide-in-right`}
          // Same opt-out as ui/Dialog: no description → no sr-only stub
          // duplicating the title; silence Radix's warning explicitly.
          {...(description ? {} : { "aria-describedby": undefined })}
        >
          <div className="flex items-start justify-between border-b border-border-default px-4 py-3">
            <div className="min-w-0">
              {title && (
                <RD.Title className="text-sm font-semibold text-fg-default">
                  {title}
                </RD.Title>
              )}
              {description && (
                <RD.Description className="text-xs text-fg-subtle mt-0.5">
                  {description}
                </RD.Description>
              )}
            </div>
            <RD.Close
              className="ml-2 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
              aria-label="Close"
            >
              <Cross2Icon />
            </RD.Close>
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">{children}</div>
          {footer && (
            <div className="flex items-center justify-end gap-2 border-t border-border-default px-4 py-3">
              {footer}
            </div>
          )}
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}

export const DrawerClose = RD.Close;
