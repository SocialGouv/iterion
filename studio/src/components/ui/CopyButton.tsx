import { type ReactNode } from "react";

import { useCopyTimer } from "@/hooks/useCopyTimer";

export interface CopyButtonProps {
  value: string;
  label?: string;
  copiedLabel?: string;
  // What this button copies, for the accessible name only: a row of copy
  // buttons all announcing "copy" is one control as far as a screen reader
  // is concerned (RGAA 11.1 / WCAG 2.4.6). The visible label is untouched
  // and stays the start of the name, which is what SC 2.5.3 asks for.
  describes?: string;
  // Visual variant. "text" is the lightweight inline pill used in
  // headers/summaries; "icon" renders a small icon-only button; "share"
  // renders a share-arrow glyph (semantically a copy of a URL).
  variant?: "text" | "icon" | "share";
  className?: string;
  // Optional callback after a successful copy (for analytics / chained UX).
  onCopied?: () => void;
}

// CopyButton centralises clipboard interactions in the SPA. Use it instead of
// reimplementing setState+timeout+navigator.clipboard pairs inline — every
// panel that displays copyable content (prompts, responses, artifacts, tool
// payloads, errors, run IDs, share links) should reach for this component so
// the "copy / copied" affordance is consistent.
//
// Errors from the Clipboard API are swallowed: the surface stays inert when
// clipboard access is denied (insecure context, permissions). Callers who need
// a fallback can pass `onCopied` and observe the state externally.
export function CopyButton({
  value,
  label = "copy",
  copiedLabel = "copied",
  describes,
  variant = "text",
  className,
  onCopied,
}: CopyButtonProps): ReactNode {
  const { copied, trigger } = useCopyTimer<boolean>(1200);

  const handleClick: React.MouseEventHandler<HTMLButtonElement> = async (e) => {
    e.stopPropagation();
    e.preventDefault();
    try {
      await navigator.clipboard.writeText(value);
      trigger(true);
      onCopied?.();
    } catch {
      // clipboard unavailable (insecure context, denied permission)
    }
  };

  const baseClass =
    variant === "icon" || variant === "share"
      ? "inline-flex items-center justify-center h-5 w-5 rounded text-fg-subtle hover:text-fg-default hover:bg-surface-2 transition-colors"
      : "text-caption text-fg-subtle hover:text-fg-default px-1";

  let inner: ReactNode;
  if (variant === "icon") {
    inner = copied ? <CheckGlyph /> : <ClipboardGlyph />;
  } else if (variant === "share") {
    inner = copied ? <CheckGlyph /> : <ShareGlyph />;
  } else {
    inner = copied ? copiedLabel : label;
  }

  const visible = copied ? copiedLabel : label;
  // Qualify the RESTING label: a name that becomes "Copied of plan" for the
  // 1.2 s of the copied state is both ungrammatical and a name change on a
  // focused control, which assistive tech re-announces.
  const name = describes ? `${label} ${describes}` : visible;

  return (
    <button
      type="button"
      onClick={handleClick}
      // The tooltip keeps the visible wording ("copy" / "copied"): the
      // qualification is for the accessible name, and "copied of plan" is not
      // English anyone should read.
      title={visible}
      aria-label={name}
      className={[baseClass, className ?? ""].join(" ")}
    >
      {inner}
    </button>
  );
}

function ClipboardGlyph() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="12"
      height="12"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="4.5" y="2.5" width="7" height="2" rx="0.6" />
      <path d="M5.5 4.5h5a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1H5.5a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1z" />
    </svg>
  );
}

function CheckGlyph() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="12"
      height="12"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M3.5 8.5l3 3 6-6" />
    </svg>
  );
}

function ShareGlyph() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="12"
      height="12"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M10 6V3l4 4-4 4V8H6.5a3 3 0 0 0-3 3v1" />
      <path d="M2.5 6.5V13a1 1 0 0 0 1 1H11" />
    </svg>
  );
}
