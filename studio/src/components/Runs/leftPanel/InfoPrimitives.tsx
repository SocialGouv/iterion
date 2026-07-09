// Shared primitives for the LeftPanel's info-style tabs (Overview + Info).
// Kept in one place so both panels render identical row/section chrome —
// the operator moves between them expecting the same visual language.
import { type ReactNode } from "react";

import { CopyIcon } from "@radix-ui/react-icons";
import { ChevronRight } from "lucide-react";

import { Tooltip } from "@/components/ui";
import { useCopyTimer } from "@/hooks/useCopyTimer";

// Section is a labelled block. `headerRight` slots optional trailing
// controls in the title row (e.g. a copy button). When `collapsible`,
// it wraps a native <details> with a rotating chevron so a long section
// (a briefing prompt, launch config, advanced details) can be folded
// away; `defaultOpen` sets the initial state. An onClickCapture guard on
// `headerRight` stops that control (e.g. a copy button) from toggling the
// section when clicked.
export function Section({
  title,
  children,
  headerRight,
  collapsible = false,
  defaultOpen = true,
}: {
  title: string;
  children: ReactNode;
  headerRight?: ReactNode;
  collapsible?: boolean;
  defaultOpen?: boolean;
}) {
  if (collapsible) {
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
            <span className="shrink-0" onClickCapture={(e) => e.preventDefault()}>
              {headerRight}
            </span>
          )}
        </summary>
        <div className="space-y-1 mt-1">{children}</div>
      </details>
    );
  }
  return (
    <section>
      <div className="flex items-center justify-between mb-1">
        <h3 className="text-caption font-semibold uppercase tracking-wide text-fg-muted">
          {title}
        </h3>
        {headerRight}
      </div>
      <div className="space-y-1">{children}</div>
    </section>
  );
}

// Row is one label/value pair inside a Section. Values truncate by
// default to keep the panel readable in the 320px-wide expanded state.
export function Row({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="grid grid-cols-[80px_1fr] gap-2 text-micro">
      <span className="text-fg-subtle truncate">{label}</span>
      <div className="min-w-0 truncate text-fg-default">{children}</div>
    </div>
  );
}

export interface MonoProps {
  children: string;
  copyable?: boolean;
  title?: string;
}

// Mono renders a compact single-line monospaced value; `copyable`
// swaps in a click-to-copy affordance with a small clipboard glyph.
export function Mono({ children, copyable, title }: MonoProps) {
  const { copied, trigger } = useCopyTimer<boolean>(1500);
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(children);
      trigger(true);
    } catch {
      // clipboard unavailable in insecure contexts — silent
    }
  };
  if (!copyable) {
    return (
      <code className="font-mono text-caption text-fg-default" title={title}>
        {children}
      </code>
    );
  }
  return (
    <Tooltip content={copied ? "Copied" : title ?? "Click to copy"}>
      <button
        type="button"
        onClick={() => void onCopy()}
        className="inline-flex items-center gap-1 font-mono text-caption text-fg-default hover:text-info"
      >
        <span className="truncate">{children}</span>
        <CopyIcon className="h-3 w-3 shrink-0 text-fg-subtle" />
      </button>
    </Tooltip>
  );
}
