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
  onOpen: () => void;
  /** Anonymous (not signed-in) viewers can't install into a workspace —
   *  they get a public `.botz` download instead. */
  anonymous?: boolean;
}

/** MarketplaceCard renders one entry as a compact tile. The card is
 *  clickable (opens the detail panel); the action buttons stop
 *  propagation so they don't double-trigger the open. Styled to match
 *  the catalog/launch surfaces' design tokens. */
export function MarketplaceCard({
  entry,
  state,
  installing,
  onInstall,
  onUpdate,
  onUninstall,
  onOpen,
  anonymous = false,
}: Props) {
  const label = entry.display_name?.trim() || entry.name;
  return (
    <li
      className="flex h-full flex-col gap-2 rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 shadow-[var(--shadow-sm)] transition-[box-shadow,border-color,transform] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)] focus-within:border-border-strong"
    >
      <button
        type="button"
        onClick={onOpen}
        className="flex flex-col items-start gap-1 text-left rounded focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
      >
        <div className="flex w-full items-baseline justify-between gap-2">
          <span className="truncate text-sm font-medium text-fg-default">{label}</span>
          <span className="shrink-0 text-caption text-fg-subtle">
            {entry.installs} install{entry.installs === 1 ? "" : "s"}
          </span>
        </div>
        {entry.description && (
          <p className="line-clamp-2 text-xs text-fg-muted">{entry.description}</p>
        )}
        <div className="flex flex-wrap items-center gap-1 text-caption text-fg-subtle">
          {entry.author && <span>by {entry.author}</span>}
          {entry.version && <span className="font-mono">v{entry.version}</span>}
          {entry.presets && entry.presets.length > 0 && (
            <span>
              {entry.presets.length} preset{entry.presets.length === 1 ? "" : "s"}
            </span>
          )}
        </div>
        {entry.tags && entry.tags.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {entry.tags.map((t) => (
              <span
                key={t}
                className="rounded bg-surface-1 px-1.5 py-0.5 text-caption text-fg-muted"
              >
                {t}
              </span>
            ))}
          </div>
        )}
      </button>
      <div className="flex items-center justify-between gap-2">
        <span className="truncate font-mono text-caption text-fg-subtle">
          {entry.repo_url}
          {entry.ref ? `#${entry.ref}` : ""}
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
      </div>
    </li>
  );
}
