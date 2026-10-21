import { useEffect, useRef } from "react";
import { AlertCircle, AlertTriangle, CheckCircle2, Info, X, type LucideIcon } from "lucide-react";

import { useUIStore, type Toast } from "@/store/ui";

const TYPE_STYLES: Record<Toast["type"], string> = {
  success: "bg-success-soft text-success-fg border-success",
  error: "bg-danger-soft text-danger-fg border-danger",
  info: "bg-accent-soft text-info-fg border-accent",
  warning: "bg-warning-soft text-warning-fg border-warning",
};

// Lucide icons matching InlineBanner's tone glyphs so toasts and inline
// banners read as one family (error reuses danger's AlertCircle).
const TYPE_ICONS: Record<Toast["type"], LucideIcon> = {
  success: CheckCircle2,
  error: AlertCircle,
  info: Info,
  warning: AlertTriangle,
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

  // Escape dismisses the most-recent toast. The listener registers once
  // and reads `toasts` via a ref so it doesn't churn on every toast
  // add/remove. The container is a region landmark, not a focus trap,
  // so Escape doesn't interfere with modal dialogs above them (Radix
  // grabs the event first inside any open modal).
  const toastsRef = useRef(toasts);
  toastsRef.current = toasts;
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      const list = toastsRef.current;
      const top = list[list.length - 1];
      if (top) removeToast(top.id);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [removeToast]);

  if (toasts.length === 0) return null;

  return (
    <div
      role="region"
      aria-label="Notifications"
      className="fixed bottom-4 right-4 z-[var(--z-toast)] flex flex-col gap-2"
    >
      {toasts.map((toast) => {
        const { role, live } = roleFor(toast.type);
        const Icon = TYPE_ICONS[toast.type];
        return (
          <div
            key={toast.id}
            role={role}
            aria-live={live}
            className={`flex items-center gap-2 px-3 py-2 rounded-lg border shadow-[var(--shadow-md)] text-sm animate-fade-in ${TYPE_STYLES[toast.type]}`}
          >
            <Icon className="h-4 w-4 shrink-0" aria-hidden />
            <span>{toast.message}</span>
            {toast.action && (
              // currentColor border + transparent fill renders consistently
              // across the four tone surfaces (success/error/info/warning)
              // in both light and dark themes — the previous bg-white/20
              // disappeared on the success-soft / warning-soft toasts
              // because the tinted background is already near-white.
              <button
                type="button"
                className="ml-2 px-2 py-0.5 rounded border border-current/40 bg-current/10 hover:bg-current/20 text-xs font-medium focus-visible:ring-1 focus-visible:ring-current"
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
              type="button"
              className="ml-1 text-xs rounded hover:bg-scrim-soft focus-visible:ring-1 focus-visible:ring-current"
              onClick={() => removeToast(toast.id)}
              aria-label="Dismiss notification"
              title="Dismiss"
            >
              <X className="h-3 w-3" aria-hidden />
            </button>
          </div>
        );
      })}
    </div>
  );
}
