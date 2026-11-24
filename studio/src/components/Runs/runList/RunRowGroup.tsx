import type { ReactNode } from "react";

// RunRowGroup renders one group's rows inside the desktop <table>. When
// the active group key is "none", the header row is suppressed and
// only the rows pass through — keeps the table identical to the
// pre-feature layout.
export function RunRowGroup({
  label,
  count,
  showHeader,
  columnSpan,
  children,
}: {
  label: string;
  count: number;
  showHeader: boolean;
  columnSpan: number;
  children: ReactNode;
}) {
  // Each group owns its <tbody>: the rowgroup header (scope="rowgroup")
  // then spans exactly the rows of ITS group, and assistive tech hears the
  // group boundary instead of one undivided body (RGAA 5.7, #2509).
  return (
    <tbody>
      {showHeader && (
        <tr className="bg-surface-2 border-y border-border-default">
          <th
            colSpan={columnSpan}
            scope="rowgroup"
            className="text-left px-4 py-1.5 font-medium text-fg-muted text-micro uppercase tracking-wide"
          >
            <span>{label}</span>
            <span className="ml-2 text-fg-subtle normal-case tracking-normal">
              {count}
            </span>
          </th>
        </tr>
      )}
      {children}
    </tbody>
  );
}
