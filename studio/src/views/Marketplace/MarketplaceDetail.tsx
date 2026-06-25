import { useEffect, useId, useRef } from "react";
import { Cross2Icon } from "@radix-ui/react-icons";

import type { MarketplaceEntry } from "@/api/marketplace";
import type { InstalledState } from "./installState";
import { InstallControls } from "./InstallControls";
import { DownloadBotz } from "./DownloadBotz";

interface Props {
  entry: MarketplaceEntry;
  state: InstalledState;
  installing: boolean;
  onInstall: () => void;
  onUpdate: () => void;
  onUninstall: () => void;
  onClose: () => void;
  /** Anonymous viewers download the `.botz` instead of installing. */
  anonymous?: boolean;
}

/** MarketplaceDetail is the right-side drawer that opens when the
 *  operator clicks a card. Shows the README + preset list so they can
 *  decide before clicking Install. Implemented as a fixed-overlay panel
 *  rather than a Dialog to keep the card list visible alongside, but
 *  carries the same keyboard / aria semantics (role=dialog, aria-modal,
 *  Escape closes, focus moves into the panel on open and restores on
 *  close). */
export function MarketplaceDetail({
  entry,
  state,
  installing,
  onInstall,
  onUpdate,
  onUninstall,
  onClose,
  anonymous = false,
}: Props) {
  const label = entry.display_name?.trim() || entry.name;
  const titleId = useId();
  const panelRef = useRef<HTMLElement | null>(null);
  // Move focus into the panel on open, restore to whatever had focus
  // before the drawer mounted on close.
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    panelRef.current?.focus();
    return () => {
      prev?.focus?.();
    };
  }, []);
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [onClose]);
  return (
    <div
      className="fixed inset-0 z-[var(--z-modal)] flex justify-end bg-scrim-popover"
      onClick={onClose}
    >
      <aside
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="flex h-full w-full max-w-xl flex-col bg-surface-1 shadow-[var(--shadow-lg)] outline-none"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex items-start justify-between gap-3 border-b border-border-default px-4 py-3">
          <div className="min-w-0 flex-1">
            <h2 id={titleId} className="truncate text-sm font-semibold text-fg-default">{label}</h2>
            <p className="truncate font-mono text-caption text-fg-subtle">
              {entry.slug}
              {entry.version ? ` · v${entry.version}` : ""}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded p-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
            aria-label="Close detail"
          >
            <Cross2Icon className="h-3.5 w-3.5" />
          </button>
        </header>

        <div className="flex-1 overflow-y-auto px-4 py-3">
          <section className="space-y-2 text-xs text-fg-default">
            {entry.description && (
              <p className="text-fg-muted">{entry.description}</p>
            )}
            <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-micro text-fg-muted">
              {entry.author && (
                <>
                  <dt className="text-fg-subtle">Author</dt>
                  <dd className="truncate text-fg-default">{entry.author}</dd>
                </>
              )}
              <dt className="text-fg-subtle">Repo</dt>
              <dd className="truncate font-mono text-fg-default">
                {entry.repo_url}
                {entry.ref ? `#${entry.ref}` : ""}
                {entry.subpath ? ` (${entry.subpath})` : ""}
              </dd>
              <dt className="text-fg-subtle">Installs</dt>
              <dd className="text-fg-default">{entry.installs}</dd>
            </dl>
            {entry.tags && entry.tags.length > 0 && (
              <div className="flex flex-wrap gap-1">
                {entry.tags.map((t) => (
                  <span
                    key={t}
                    className="rounded bg-surface-2 px-1.5 py-0.5 text-caption text-fg-muted"
                  >
                    {t}
                  </span>
                ))}
              </div>
            )}
          </section>

          {entry.presets && entry.presets.length > 0 && (
            <section className="mt-4 space-y-1">
              <h3 className="text-caption uppercase tracking-wide text-fg-subtle">
                Presets ({entry.presets.length})
              </h3>
              <ul className="space-y-1">
                {entry.presets.map((p) => (
                  <li
                    key={p.name}
                    className="rounded border border-border-default bg-surface-2 p-2 text-xs"
                  >
                    <div className="flex items-baseline justify-between gap-2">
                      <span className="truncate font-medium text-fg-default">
                        {p.display_name || p.name}
                      </span>
                      <span className="shrink-0 font-mono text-caption text-fg-subtle">
                        {p.name}
                      </span>
                    </div>
                    {p.description && (
                      <p className="mt-0.5 text-micro text-fg-muted">{p.description}</p>
                    )}
                    {p.skills && p.skills.length > 0 && (
                      <div className="mt-1 flex flex-wrap gap-1 text-caption text-fg-subtle">
                        {p.skills.map((s) => (
                          <span key={s} className="rounded bg-surface-1 px-1 py-0.5">
                            {s}
                          </span>
                        ))}
                      </div>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          )}

          {entry.readme && (
            <section className="mt-4 space-y-1">
              <h3 className="text-caption uppercase tracking-wide text-fg-subtle">README</h3>
              <pre className="max-h-96 overflow-y-auto whitespace-pre-wrap rounded border border-border-default bg-surface-2 p-3 font-mono text-micro text-fg-default">
                {entry.readme}
              </pre>
            </section>
          )}
        </div>

        <footer className="flex items-center justify-between gap-2 border-t border-border-default px-4 py-3">
          <span className="text-caption text-fg-subtle">
            {anonymous ? (
              <>
                Download the <code className="text-fg-default">.botz</code> bundle, or sign in to install.
              </>
            ) : (
              <>
                Installs into <code className="text-fg-default">.botz/</code> — never run automatically.
              </>
            )}
          </span>
          {anonymous ? (
            <DownloadBotz slug={entry.slug} />
          ) : (
            <InstallControls
              state={state}
              installing={installing}
              onInstall={onInstall}
              onUpdate={onUpdate}
              onUninstall={onUninstall}
            />
          )}
        </footer>
      </aside>
    </div>
  );
}
