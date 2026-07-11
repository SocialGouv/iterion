import { Spinner } from "@/components/ui/Spinner";

// Loading slate for a panel/tab section: centered spinner filling the
// section's height. Completes the loading vocabulary next to
// BootLoading (pre-shell, h-screen) and MainSpinner (route Suspense
// inside <main>) — this is the one panels and settings tabs reach for
// while their fetch is pending.
export default function PanelLoading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex h-full items-center justify-center px-3 py-8 text-fg-subtle">
      <Spinner size="sm" label={label} />
    </div>
  );
}
