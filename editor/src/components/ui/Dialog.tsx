import * as RD from "@radix-ui/react-dialog";
import { Cross2Icon } from "@radix-ui/react-icons";
import type { ReactNode } from "react";

export interface DialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title?: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  /** Tailwind width class. */
  widthClass?: string;
  /** Hide the default close button (e.g., for confirm dialogs that own their actions). */
  hideClose?: boolean;
}

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  widthClass = "max-w-lg",
  hideClose = false,
}: DialogProps) {
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className="fixed inset-0 z-40 bg-black/60 animate-fade-in" />
        <RD.Content
          className={`fixed left-1/2 top-1/2 z-50 w-[calc(100vw-2rem)] ${widthClass} -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border-default bg-surface-1 text-fg-default shadow-2xl animate-fade-in`}
        >
          {(title || !hideClose) && (
            <div className="flex items-start justify-between border-b border-border-default px-4 py-3">
              <div className="min-w-0">
                {title && (
                  <RD.Title className="text-sm font-semibold text-fg-default">
                    {title}
                  </RD.Title>
                )}
                {/* Radix Dialog requires a Description (or
                    aria-describedby) on every Content for screen
                    readers; render visibly when one is provided,
                    else fall back to a sr-only stub so the warning
                    doesn't fire on simple dialogs that already
                    convey their intent through the title. */}
                {description ? (
                  <RD.Description className="text-xs text-fg-subtle mt-0.5">
                    {description}
                  </RD.Description>
                ) : (
                  <RD.Description className="sr-only">
                    {typeof title === "string" ? title : "Dialog"}
                  </RD.Description>
                )}
              </div>
              {!hideClose && (
                <RD.Close
                  className="ml-2 inline-flex h-6 w-6 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
                  aria-label="Close"
                >
                  <Cross2Icon />
                </RD.Close>
              )}
            </div>
          )}
          <div className="px-4 py-3">{children}</div>
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

export const DialogClose = RD.Close;
