import { useEffect, useRef } from "react";
import { createPortal } from "react-dom";

interface Props {
  open: boolean;
  title: string;
  message: string;
  confirmLabel?: string;
  confirmVariant?: "danger" | "default";
  onConfirm: () => void;
  onCancel: () => void;
}

export default function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = "Confirm",
  confirmVariant = "default",
  onConfirm,
  onCancel,
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

  const confirmClass =
    confirmVariant === "danger"
      ? "bg-danger hover:bg-danger text-fg-default"
      : "bg-accent hover:bg-accent-hover text-fg-default";

  // Portal to document.body and pin z-[60] so the dialog always stacks
  // above a parent modal that opened it. Inline rendering at z-50 lost
  // the DOM-order tiebreaker against Radix's body-portaled Dialog (also
  // z-50), making the confirm appear behind the ProjectSwitcher modal.
  const content = (
    <div className="fixed inset-0 z-[60] bg-black/50 flex items-center justify-center">
      <div className="bg-surface-1 border border-border-strong rounded-lg p-4 min-w-[300px] max-w-[400px]">
        <h3 className="text-sm font-bold text-fg-default mb-2">{title}</h3>
        <p className="text-xs text-fg-muted mb-4">{message}</p>
        <div className="flex justify-end gap-2">
          <button
            ref={cancelRef}
            className="bg-surface-2 hover:bg-surface-3 px-3 py-1.5 rounded text-xs text-fg-default"
            onClick={onCancel}
          >
            Cancel
          </button>
          <button
            className={`px-3 py-1.5 rounded text-xs ${confirmClass}`}
            onClick={onConfirm}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );

  if (typeof document === "undefined") return content;
  return createPortal(content, document.body);
}
