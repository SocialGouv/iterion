import { Skeleton } from "@/components/ui/Skeleton";

// BoardSkeleton is the initial-load placeholder: four column-shaped
// stacks of card skeletons matching the real board's layout metrics
// (w-72 columns, gap-3) so the swap-in doesn't shift the layout.
export function BoardSkeleton() {
  return (
    // role="status": a roleless div may not carry aria-label (ARIA in HTML,
    // #2767); the status role announces the loading state and the sr-only
    // text survives stylesheets being disabled.
    <div
      className="h-full flex flex-col overflow-hidden"
      role="status"
    >
      <span className="sr-only">Loading board</span>
      <div className="flex-1 flex gap-3 overflow-hidden p-3">
        {Array.from({ length: 4 }).map((_, c) => (
          <div key={c} className="flex w-72 shrink-0 flex-col gap-2">
            <Skeleton className="h-6 w-32" />
            {Array.from({ length: 3 }).map((__, k) => (
              <Skeleton key={k} className="h-16 w-full" />
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}
