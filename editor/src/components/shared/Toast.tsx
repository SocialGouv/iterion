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

export default function ToastContainer() {
  const toasts = useUIStore((s) => s.toasts);
  const removeToast = useUIStore((s) => s.removeToast);

  if (toasts.length === 0) return null;

  return (
    <div className="fixed bottom-4 right-4 z-[100] flex flex-col gap-2">
      {toasts.map((toast) => (
        <div
          key={toast.id}
          className={`flex items-center gap-2 px-3 py-2 rounded-lg border shadow-lg text-sm animate-fade-in ${TYPE_STYLES[toast.type]}`}
        >
          <span className="font-bold">{TYPE_ICONS[toast.type]}</span>
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
            title="Dismiss"
          >
            &#x2715;
          </button>
        </div>
      ))}
    </div>
  );
}
