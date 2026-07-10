import { useRef, type ReactNode } from "react";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";

interface SecondaryAction {
  label: string;
  onClick: () => void;
  variant?: "default" | "danger";
}

interface Props {
  open: boolean;
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  confirmVariant?: "danger" | "default";
  onConfirm: () => void;
  onCancel: () => void;
  // Optional middle button — useful when the dialog has three real
  // outcomes ("Cancel" / "Do X" / "Do Y") rather than the default
  // confirm-or-cancel split. Sits between Cancel and the primary
  // action so the visual reading order is left→right destructive.
  secondaryAction?: SecondaryAction;
}

// Thin wrapper over ui/Dialog (Radix): focus trap, focus restore, Escape
// and overlay-click dismissal all come from Radix. stack="confirm" pins
// overlay + content at --z-confirm so the dialog always stacks above a
// parent modal that opened it.
export default function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = "Confirm",
  confirmVariant = "default",
  onConfirm,
  onCancel,
  secondaryAction,
}: Props) {
  const cancelRef = useRef<HTMLButtonElement>(null);

  // Strings render inside a <p> for the historical layout; ReactNode
  // bodies (multi-paragraph, inline strong, etc) render inside a div
  // so callers can supply their own structure.
  const messageNode =
    typeof message === "string" ? (
      <p className="text-xs text-fg-muted">{message}</p>
    ) : (
      <div className="text-xs text-fg-muted space-y-2">{message}</div>
    );

  return (
    <Dialog
      open={open}
      // Radix reports Escape and overlay clicks as onOpenChange(false);
      // both mean "don't do it" here.
      onOpenChange={(v) => {
        if (!v) onCancel();
      }}
      title={title}
      stack="confirm"
      hideClose
      widthClass="max-w-[440px]"
      // Initial focus lands on Cancel (least-destructive), not the
      // first tabbable element Radix would pick.
      onOpenAutoFocus={(e) => {
        e.preventDefault();
        cancelRef.current?.focus();
      }}
      footer={
        <>
          <Button ref={cancelRef} variant="secondary" size="sm" onClick={onCancel}>
            Cancel
          </Button>
          {secondaryAction && (
            <Button
              variant={secondaryAction.variant === "danger" ? "danger" : "secondary"}
              size="sm"
              onClick={secondaryAction.onClick}
            >
              {secondaryAction.label}
            </Button>
          )}
          <Button
            variant={confirmVariant === "danger" ? "danger" : "primary"}
            size="sm"
            onClick={onConfirm}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      {messageNode}
    </Dialog>
  );
}
