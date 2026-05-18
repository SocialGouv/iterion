import { useEffect, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";

import { Button } from "@/components/ui/Button";

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

  useEffect(() => {
    if (open) cancelRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open, onCancel]);

  if (!open) return null;

  // Strings render inside a <p> for the historical layout; ReactNode
  // bodies (multi-paragraph, inline strong, etc) render inside a div
  // so callers can supply their own structure.
  const messageNode =
    typeof message === "string" ? (
      <p className="text-xs text-fg-muted mb-4">{message}</p>
    ) : (
      <div className="text-xs text-fg-muted mb-4 space-y-2">{message}</div>
    );

  // Portal to document.body and pin z-[var(--z-confirm)] so the dialog
  // always stacks above a parent modal that opened it. The semantic
  // ladder lives in app.css @theme.
  const content = (
    <div className="fixed inset-0 z-[var(--z-confirm)] bg-black/50 flex items-center justify-center">
      <div className="bg-surface-1 border border-border-strong rounded-lg p-4 min-w-[300px] max-w-[440px]">
        <h3 className="text-sm font-bold text-fg-default mb-2">{title}</h3>
        {messageNode}
        <div className="flex justify-end gap-2">
          <Button
            ref={cancelRef}
            variant="secondary"
            size="sm"
            onClick={onCancel}
          >
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
        </div>
      </div>
    </div>
  );

  if (typeof document === "undefined") return content;
  return createPortal(content, document.body);
}
