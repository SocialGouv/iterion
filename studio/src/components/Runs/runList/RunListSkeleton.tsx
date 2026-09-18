import { Skeleton } from "@/components/ui/Skeleton";

// Cold-load placeholder for the runs list. Shown only on the very first
// load of a scope (no cached data yet); subsequent scope switches keep
// the previous list visible with a dim overlay instead (keepPreviousData).
//
// Mirrors RunListView's two layouts so there is no jump when the real
// rows land: the semantic <table> (8 columns, matching RunListRow) on
// sm+ and the stacked cards on mobile.

interface Props {
  // How many placeholder rows to render. Defaults to a screenful.
  rows?: number;
}

const DEFAULT_ROWS = 8;

export function RunListSkeleton({ rows = DEFAULT_ROWS }: Props) {
  const items = Array.from({ length: rows });
  return (
    <div
      className="w-full"
      role="status"
      aria-live="polite"
      aria-busy="true"
      aria-label="Loading runs"
    >
      {/* Desktop / tablet: mirror the real table so columns don't shift. */}
      <table className="w-full text-xs hidden sm:table">
        <caption className="sr-only">Loading runs</caption>
        <tbody>
          {items.map((_, i) => (
            <tr key={i} className="border-b border-border-default">
              <td className="pl-4 pr-1 py-2 w-8">
                <Skeleton className="h-3.5 w-3.5 rounded-sm" />
              </td>
              <td className="px-4 py-2">
                <div className="flex items-center gap-2">
                  <Skeleton className="h-5 w-5 rounded-full" />
                  <Skeleton className="h-3 w-40" />
                </div>
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-3 w-28" />
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-4 w-16 rounded-full" />
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-4 w-20 rounded-full" />
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-3 w-16" />
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-3 w-12" />
              </td>
              <td className="px-4 py-2">
                <Skeleton className="h-3 w-14" />
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {/* Mobile: stacked cards mirroring RunListCard's rough footprint. */}
      <ul className="sm:hidden divide-y divide-border-default">
        {items.map((_, i) => (
          <li key={i} className="px-4 py-3">
            <div className="flex items-center gap-2">
              <Skeleton className="h-6 w-6 rounded-full" />
              <Skeleton className="h-3 w-40" />
              <Skeleton className="ml-auto h-4 w-16 rounded-full" />
            </div>
            <div className="mt-2 flex items-center gap-3">
              <Skeleton className="h-3 w-24" />
              <Skeleton className="h-3 w-16" />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
