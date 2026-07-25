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
  /** Stacking layer: "confirm" pins the dialog above other open modals (--z-confirm). */
  stack?: "modal" | "confirm";
  /** Radix open-autofocus hook — call event.preventDefault() and focus a
   *  specific element to override the default focus-first-tabbable. */
  onOpenAutoFocus?: (event: Event) => void;
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
  stack = "modal",
  onOpenAutoFocus,
}: DialogProps) {
  // "confirm" puts overlay AND content at --z-confirm; the content paints
  // above its own overlay by DOM order, and both sit above --z-modal.
  const overlayZ = stack === "confirm" ? "z-[var(--z-confirm)]" : "z-[var(--z-overlay)]";
  const contentZ = stack === "confirm" ? "z-[var(--z-confirm)]" : "z-[var(--z-modal)]";
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Portal>
        <RD.Overlay className={`fixed inset-0 ${overlayZ} bg-scrim-modal animate-fade-in`} />
        <RD.Content
          className={`fixed left-1/2 top-1/2 ${contentZ} flex max-h-[calc(100vh-2rem)] w-[calc(100vw-2rem)] ${widthClass} -translate-x-1/2 -translate-y-1/2 flex-col rounded-lg border border-border-default bg-surface-1 text-fg-default shadow-[var(--shadow-lg)] animate-fade-in`}
          onOpenAutoFocus={onOpenAutoFocus}
          // Radix warns unless every Content has a Description or an
          // explicit aria-describedby={undefined}. Dialogs without a
          // description opt out rather than duplicating the title in a
          // sr-only stub (screen readers already announce the title).
          {...(description ? {} : { "aria-describedby": undefined })}
        >
          {(title || !hideClose) && (
            <div className="flex shrink-0 items-start justify-between border-b border-border-default px-4 py-3">
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
          <div className="flex-1 overflow-y-auto px-4 py-3">{children}</div>
          {footer && (
            <div className="flex shrink-0 items-center justify-end gap-2 border-t border-border-default px-4 py-3">
              {footer}
            </div>
          )}
        </RD.Content>
      </RD.Portal>
    </RD.Root>
  );
}

export const DialogClose = RD.Close;
