import type { ReactNode } from "react";

import { ChevronRightIcon } from "@radix-ui/react-icons";

// Compact disclosure for the secondary run metadata rendered between the
// identity/actions header and the metrics bar. A native <details> keeps the
// contents mounted while collapsed, so loaded children and note drafts survive
// an open/close cycle without occupying diagram space.
export default function RunInformationAccordion({
  children,
}: {
  children: ReactNode;
}) {
  return (
    <details className="group shrink-0">
      <summary className="flex cursor-pointer list-none select-none items-center gap-1.5 border-t border-border-default px-3 py-1.5 text-micro font-medium uppercase tracking-wide text-fg-muted transition-colors hover:bg-surface-2/40 hover:text-fg-default focus:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent sm:px-4 [&::-webkit-details-marker]:hidden">
        <ChevronRightIcon
          className="h-3 w-3 shrink-0 text-fg-subtle transition-transform duration-[var(--motion-fast)] group-open:rotate-90"
          aria-hidden
        />
        Run information
      </summary>
      <div className="border-t border-border-default">{children}</div>
    </details>
  );
}
