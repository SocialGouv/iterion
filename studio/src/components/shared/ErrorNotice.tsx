import { InlineBanner, type InlineBannerTone } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { errorHint, errorMessage } from "@/lib/errorHints";

// ErrorNotice is the inline counterpart to toastError: a token-styled
// banner for an error rendered inside a dialog/panel slot. It maps the
// raw error to a friendly headline + what-to-do hint (via errorHints),
// keeps the original message recoverable in a collapsed <details>, and
// offers an optional Retry button. Replaces the scattered raw
// `text-red-* {err.message}` divs with one consistent, accessible shape.
export interface ErrorNoticeProps {
  error: unknown;
  // Names the operation ("Open file failed", "Fork failed"); prepended
  // to the inferred title.
  context?: string;
  onRetry?: () => void;
  retryLabel?: string;
  tone?: Extract<InlineBannerTone, "warning" | "danger">;
  className?: string;
}

export function ErrorNotice({
  error,
  context,
  onRetry,
  retryLabel = "Retry",
  tone = "danger",
  className,
}: ErrorNoticeProps) {
  const hint = errorHint(error);
  const raw = errorMessage(error);
  const base = hint?.title ?? raw;
  const headline = context ? `${context}: ${base}` : base;

  return (
    <InlineBanner
      tone={tone}
      layout="inline"
      title={headline}
      action={
        onRetry ? (
          <Button variant="secondary" size="sm" onClick={onRetry}>
            {retryLabel}
          </Button>
        ) : undefined
      }
      className={className}
    >
      {hint?.hint && <div className="mt-0.5">{hint.hint}</div>}
      {/* When a hint matched, the raw error isn't in the headline —
          keep it recoverable but out of the way. */}
      {hint && (
        <details className="mt-1">
          <summary className="cursor-pointer hover:underline">
            Details
          </summary>
          <pre className="mt-1 whitespace-pre-wrap break-words font-mono text-micro">
            {raw}
          </pre>
        </details>
      )}
    </InlineBanner>
  );
}
