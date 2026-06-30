import type { ReactNode } from "react";

export interface PageHeaderProps {
  /** Page title — rendered as the page-level <h1> in the headline type token. */
  title: ReactNode;
  /** One-line supporting description under the title. */
  description?: ReactNode;
  /** Small uppercase eyebrow label above the title (breadcrumb / section). */
  eyebrow?: ReactNode;
  /** Optional leading glyph (a 20–24px icon). Tinted accent-soft container. */
  icon?: ReactNode;
  /** Right-aligned actions (buttons, toggles). */
  actions?: ReactNode;
  /**
   * Inner content width. `"default"` centres at max-w-5xl to match the
   * studio's content views (Marketplace, Settings, …); `"wide"` lets the
   * header span the full pane (Board, Dispatcher, dashboards).
   */
  width?: "default" | "wide";
  className?: string;
}

/**
 * PageHeader — the single shared header for content-titled views. Replaces the
 * per-view ad-hoc `<header className="border-b px-6 py-4">…</header>` blocks so
 * every page shares one rhythm: title in the `headline` type token, a muted
 * description, an optional eyebrow + icon, and a right-aligned action slot.
 *
 * This is the *content* title; it is distinct from `ContextualHeaderBar` (the
 * slim, always-present account/strip bar above <main>). A view uses one or the
 * other — not both for the same title. See docs/design-system.md § PageHeader.
 */
export function PageHeader({
  title,
  description,
  eyebrow,
  icon,
  actions,
  width = "default",
  className = "",
}: PageHeaderProps) {
  return (
    <header
      className={`shrink-0 border-b border-border-default bg-surface-1 px-6 py-4 ${className}`.trim()}
    >
      <div
        className={`mx-auto flex w-full items-start gap-4 ${
          width === "wide" ? "" : "max-w-5xl"
        }`}
      >
        {icon && (
          <div
            className="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-accent-soft text-accent-text"
            aria-hidden
          >
            {icon}
          </div>
        )}
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          {eyebrow && (
            <span className="text-caption font-medium uppercase tracking-wide text-fg-subtle">
              {eyebrow}
            </span>
          )}
          <h1 className="text-headline font-semibold leading-tight tracking-tight text-fg-default">
            {title}
          </h1>
          {description && (
            <p className="max-w-2xl text-body leading-relaxed text-fg-muted">
              {description}
            </p>
          )}
        </div>
        {actions && (
          <div className="flex shrink-0 items-center gap-2">{actions}</div>
        )}
      </div>
    </header>
  );
}
