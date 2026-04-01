import { useUIStore } from "@/store/ui";

const TYPE_STYLES = {
  success: "bg-green-900/90 text-green-200 border-green-700",
  error: "bg-red-900/90 text-red-200 border-red-700",
  info: "bg-blue-900/90 text-blue-200 border-blue-700",
};

const TYPE_ICONS = {
  success: "\u2713",
  error: "\u2716",
  info: "\u2139",
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
          onClick={() => removeToast(toast.id)}
        >
          <span className="font-bold">{TYPE_ICONS[toast.type]}</span>
          <span>{toast.message}</span>
        </div>
      ))}
    </div>
  );
}
