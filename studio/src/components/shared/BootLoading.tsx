import { Spinner } from "@/components/ui/Spinner";

// Full-viewport loading fallback for the pre-AppShell boot phase: auth
// resolution and the top-level route Suspense boundaries that render before
// the persistent shell mounts. The in-shell counterpart is MainSpinner
// (h-full, sits inside <main>). Replaces the ad-hoc top-left "Loading…" divs
// with one centred spinner so the boot phase reads as deliberate. label names
// what is pending when it is not the boot itself (a sign-out).
export default function BootLoading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="h-screen flex items-center justify-center gap-2 bg-surface-0 text-fg-muted text-xs">
      <Spinner size="sm" label={label} />
      <span>{label}…</span>
    </div>
  );
}
