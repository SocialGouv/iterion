import { useEffect } from "react";

import { useUIStore, type Toast } from "@/store/ui";

const TYPE_STYLES: Record<Toast["type"], string> = {
  success: "bg-success-soft text-success-fg border-success",
  error: "bg-danger-soft text-danger-fg border-danger",
  info: "bg-accent-soft text-info-fg border-accent",
  warning: "bg-warning-soft text-warning-fg border-warning",
};

const TYPE_ICONS: Record<Toast["type"], string> = {
  success: "\u2713",
  error: "\u2716",
  info: "\u2139",
  warning: "\u26A0",
};

// Per-toast a11y role:
//   - error  \u2192 role="alert" + aria-live="assertive" (interrupts the AT
//              queue; reserved for failures the user must notice).
//   - others \u2192 role="status" + aria-live="polite" (announces after the
//              current AT utterance finishes).
function roleFor(level: Toast["type"]): { role: "alert" | "status"; live: "assertive" | "polite" } {
  if (level === "error") return { role: "alert", live: "assertive" };
  return { role: "status", live: "polite" };
}

export default function ToastContainer() {
  const toasts = useUIStore((s) => s.toasts);
  const removeToast = useUIStore((s) => s.removeToast);

  // Escape dismisses the most-recent toast. The container is a region
  // landmark, not a focus trap, so Escape doesn't interfere with modal
  // dialogs above (they grab Escape before this fires through Radix).
  useEffect(() => {
    if (toasts.length === 0) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      const top = toasts[toasts.length - 1];
      if (top) removeToast(top.id);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [toasts, removeToast]);

  if (toasts.length === 0) return null;

  return (
    <div
      role="region"
      aria-label="Notifications"
      className="fixed bottom-4 right-4 z-[var(--z-toast)] flex flex-col gap-2"
    >
      {toasts.map((toast) => {
        const { role, live } = roleFor(toast.type);
        return (
          <div
            key={toast.id}
            role={role}
            aria-live={live}
            className={`flex items-center gap-2 px-3 py-2 rounded-lg border shadow-lg text-sm animate-fade-in ${TYPE_STYLES[toast.type]}`}
          >
            <span className="font-bold" aria-hidden="true">{TYPE_ICONS[toast.type]}</span>
            <span>{toast.message}</span>
            {toast.action && (
              <button
                className="ml-2 px-2 py-0.5 rounded bg-white/20 hover:bg-white/30 text-xs font-medium"
                onClick={(e) => {
                  e.stopPropagation();
                  toast.action!.onClick();
                  removeToast(toast.id);
                }}
              >
                {toast.action.label}
              </button>
            )}
            <button
              className="ml-1 opacity-60 hover:opacity-100 text-xs"
              onClick={() => removeToast(toast.id)}
              aria-label="Dismiss notification"
              title="Dismiss"
            >
              &#x2715;
            </button>
          </div>
        );
      })}
    </div>
  );
}
