import { type ReactNode } from "react";

import { ChevronRight } from "lucide-react";

// OverviewSection is the collapsible sibling of InfoPrimitives.Section:
// the same uppercase-caption eyebrow, wrapped in a native <details> so a
// section (long briefing, launch config, advanced details) can be folded
// away to keep the meters and progress above the fold. Native <details>
// keeps it keyboard-accessible with no extra ARIA wiring. `headerRight`
// slots a control (e.g. a copy button) into the summary; an onClickCapture
// preventDefault stops that control's click from toggling the section.
export function OverviewSection({
  title,
  children,
  headerRight,
  defaultOpen = true,
}: {
  title: string;
  children: ReactNode;
  headerRight?: ReactNode;
  defaultOpen?: boolean;
}) {
  return (
    <details className="group" open={defaultOpen}>
      <summary className="flex items-center justify-between gap-2 mb-1 cursor-pointer list-none select-none [&::-webkit-details-marker]:hidden">
        <span className="inline-flex items-center gap-1 text-caption font-semibold uppercase tracking-wide text-fg-muted">
          <ChevronRight
            size={12}
            className="text-fg-subtle transition-transform duration-[var(--motion-fast)] group-open:rotate-90"
            aria-hidden
          />
          {title}
        </span>
        {headerRight && (
          <span
            className="shrink-0"
            onClickCapture={(e) => e.preventDefault()}
          >
            {headerRight}
          </span>
        )}
      </summary>
      <div className="space-y-1 mt-1">{children}</div>
    </details>
  );
}
