import type { ReactNode } from "react";
import { AlertCircle, AlertTriangle, CheckCircle2, Info, X } from "lucide-react";

// InlineBanner is the single shape for soft-tinted, severity-coloured
// status banners — the sticky page-top kind (Board / Dispatcher) and
// the inset inline kind (inside cards / drawers). It replaces a family
// of near-identical hand-rolled `bg-<sev>-500/10 …` divs that drifted
// apart and bypassed the design-system tokens. Severity colour comes
// only from the semantic tokens (mirrors Badge's variantClass), so
// light/dark themes invert for free.
export type InlineBannerTone = "info" | "warning" | "danger" | "success";

export interface InlineBannerProps {
  tone: InlineBannerTone;
  // Bold leading line (e.g. "Dispatcher paused", "Tracker error").
  title?: ReactNode;
  // Body content. Rendered below the title when both are present.
  children?: ReactNode;
  // Leading icon. Defaults to a tone-appropriate glyph; pass `null`
  // to render no icon.
  icon?: ReactNode;
  // Flush-right control (e.g. a Resume / Override button).
  action?: ReactNode;
  // Trailing muted text (e.g. "(github)" in the tracker banner).
  suffix?: ReactNode;
  // Renders a trailing dismiss button wired to onDismiss.
  dismissable?: boolean;
  onDismiss?: () => void;
  // "sticky" (default): page-top strip with a bottom border.
  // "inline": inset rounded card for use inside panels.
  layout?: "sticky" | "inline";
  // Override the live-region role. Defaults to alert/assertive for
  // danger, status/polite otherwise (matches Toast.tsx).
  role?: "status" | "alert";
  className?: string;
}

const toneClass: Record<InlineBannerTone, string> = {
  info: "bg-info-soft text-info-fg border-info/40",
  warning: "bg-warning-soft text-warning-fg border-warning/40",
  danger: "bg-danger-soft text-danger-fg border-danger/40",
  success: "bg-success-soft text-success-fg border-success/40",
};

const defaultIcon: Record<InlineBannerTone, ReactNode> = {
  info: <Info className="h-4 w-4 shrink-0" aria-hidden />,
  warning: <AlertTriangle className="h-4 w-4 shrink-0" aria-hidden />,
  danger: <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />,
  success: <CheckCircle2 className="h-4 w-4 shrink-0" aria-hidden />,
};

export function InlineBanner({
  tone,
  title,
  children,
  icon,
  action,
  suffix,
  dismissable,
  onDismiss,
  layout = "sticky",
  role,
  className = "",
}: InlineBannerProps) {
  const resolvedRole = role ?? (tone === "danger" ? "alert" : "status");
  const ariaLive = resolvedRole === "alert" ? "assertive" : "polite";
  const layoutClass =
    layout === "sticky"
      ? "shrink-0 border-b px-4 py-2"
      : "rounded border px-3 py-2";
  // `icon` undefined → tone default; `icon={null}` → no icon.
  const leadingIcon = icon === undefined ? defaultIcon[tone] : icon;

  return (
    <div
      role={resolvedRole}
      aria-live={ariaLive}
      className={`flex items-start gap-2 text-xs ${toneClass[tone]} ${layoutClass} ${className}`.trim()}
    >
      {leadingIcon}
      <div className="min-w-0 flex-1">
        {title && <div className="font-medium">{title}</div>}
        {children}
      </div>
      {suffix && <span className="shrink-0">{suffix}</span>}
      {action && <div className="shrink-0">{action}</div>}
      {dismissable && (
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss"
          className="-mr-1 shrink-0 rounded p-0.5 hover:bg-scrim-soft"
        >
          <X className="h-3.5 w-3.5" aria-hidden />
        </button>
      )}
    </div>
  );
}
