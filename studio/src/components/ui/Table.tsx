import {
  createContext,
  useContext,
  type ReactNode,
  type HTMLAttributes,
  type ThHTMLAttributes,
  type TdHTMLAttributes,
} from "react";

import { Skeleton } from "./Skeleton";

// Data-table primitive: a styled composition (not a data-driven grid) so
// heterogeneous cells — buttons, tooltips, per-row status classes — stay
// plain JSX. Accessibility is baked in: `caption` is mandatory (RGAA
// 5.4/5.5) and `Th` defaults to scope="col" (RGAA 5.7).

type Align = "left" | "center" | "right";

const alignClass: Record<Align, string> = {
  left: "text-left",
  center: "text-center",
  right: "text-right",
};

type Density = "sm" | "md";

const cellPad: Record<Density, string> = {
  sm: "px-3 py-1.5",
  md: "px-2 py-2",
};

const headPad: Record<Density, string> = {
  sm: "px-3 py-1.5",
  md: "px-2 py-1",
};

const DensityContext = createContext<Density>("md");

export interface TableProps {
  /** Accessible table title — rendered as a <caption> (sr-only unless captionVisible). */
  caption: string;
  captionVisible?: boolean;
  /** md = text-sm roomy rows (default); sm = text-xs compact rows (dense dashboards). */
  density?: Density;
  className?: string;
  children: ReactNode;
}

export function Table({
  caption,
  captionVisible = false,
  density = "md",
  className = "",
  children,
}: TableProps) {
  return (
    <DensityContext.Provider value={density}>
      <div className="overflow-x-auto">
        <table
          className={`w-full ${density === "sm" ? "text-xs" : "text-sm"} ${className}`.trim()}
        >
          <caption
            className={
              captionVisible
                ? "text-left text-xs text-fg-muted pb-2"
                : "sr-only"
            }
          >
            {caption}
          </caption>
          {children}
        </table>
      </div>
    </DensityContext.Provider>
  );
}

export function THead({
  className = "",
  children,
}: {
  /** Styles the header row (e.g. bg-surface-1 header fills). */
  className?: string;
  children: ReactNode;
}) {
  return (
    <thead className="text-xs uppercase tracking-wider text-fg-muted">
      <tr className={className}>{children}</tr>
    </thead>
  );
}

export interface ThProps extends ThHTMLAttributes<HTMLTableCellElement> {
  align?: Align;
  /** Screen-reader-only label for visually empty header cells (e.g. action columns). */
  srLabel?: string;
}

export function Th({ align = "left", scope = "col", className = "", srLabel, children, ...rest }: ThProps) {
  const density = useContext(DensityContext);
  return (
    <th
      scope={scope}
      className={`${headPad[density]} font-medium ${alignClass[align]} ${className}`.trim()}
      {...rest}
    >
      {srLabel && !children ? <span className="sr-only">{srLabel}</span> : children}
    </th>
  );
}

export function TBody({ children }: { children: ReactNode }) {
  return <tbody>{children}</tbody>;
}

export interface TrProps extends HTMLAttributes<HTMLTableRowElement> {
  /** Row hover tint — on for data rows, disable for skeleton/summary rows. */
  hover?: boolean;
}

export function Tr({ hover = true, className = "", children, ...rest }: TrProps) {
  return (
    <tr
      className={`border-t border-border-subtle ${hover ? "hover:bg-surface-2/40" : ""} ${className}`.trim()}
      {...rest}
    >
      {children}
    </tr>
  );
}

export interface TdProps extends TdHTMLAttributes<HTMLTableCellElement> {
  align?: Align;
}

export function Td({ align = "left", className = "", children, ...rest }: TdProps) {
  const density = useContext(DensityContext);
  return (
    <td
      className={`${cellPad[density]} ${alignClass[align]} ${className}`.trim()}
      {...rest}
    >
      {children}
    </td>
  );
}

export interface TableSkeletonProps {
  rows?: number;
  cols?: number;
  className?: string;
}

/** Loading placeholder for a table region — pairs with EmptyState (empty)
 * and InlineBanner (error) to complete the loading/empty/error triad. */
export function TableSkeleton({ rows = 4, cols = 4, className = "" }: TableSkeletonProps) {
  return (
    <div className={`space-y-2 py-1 ${className}`.trim()} aria-hidden>
      {Array.from({ length: rows }, (_, r) => (
        <div key={r} className="flex gap-3">
          {Array.from({ length: cols }, (_, c) => (
            <Skeleton key={c} className="h-3 flex-1" />
          ))}
        </div>
      ))}
    </div>
  );
}
