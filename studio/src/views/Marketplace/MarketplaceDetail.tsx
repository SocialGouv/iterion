import type { MarketplaceEntry } from "@/api/marketplace";
import { Badge } from "@/components/ui/Badge";
import { Drawer } from "@/components/ui/Drawer";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import type { InstalledState } from "./installState";
import { InstallControls } from "./InstallControls";
import { DownloadBotz } from "./DownloadBotz";
import { CopyPluginInstall } from "./CopyPluginInstall";

interface Props {
  entry: MarketplaceEntry;
  state: InstalledState;
  installing: boolean;
  onInstall: () => void;
  onUpdate: () => void;
  onUninstall: () => void;
  onClose: () => void;
  /** Viewers without an installable workspace (anonymous, restricted,
   *  or any cloud tenant — the server refuses install in cloud mode)
   *  download the `.botz` (bots) or copy the CLI install command
   *  (plugins) instead of installing. */
  anonymous?: boolean;
  /** Signed-in cloud tenant: same affordances as anonymous, but the
   *  footer explains installs happen on a local studio instead of
   *  inviting them to "sign in". */
  cloud?: boolean;
}

/** MarketplaceDetail is the right-side drawer that opens when the
 *  operator clicks a card. Shows the README + preset list so they can
 *  decide before clicking Install. Built on ui/Drawer (Radix), which
 *  owns focus trap/restore, Escape and click-outside dismissal. */
export function MarketplaceDetail({
  entry,
  state,
  installing,
  onInstall,
  onUpdate,
  onUninstall,
  onClose,
  anonymous = false,
  cloud = false,
}: Props) {
  const label = entry.display_name?.trim() || entry.name;
  const isPlugin = (entry.kind ?? "bot") === "plugin";
  return (
    <Drawer
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={
        <span className="flex min-w-0 items-center gap-1.5">
          {entry.icon && (
            <span aria-hidden className="shrink-0 text-base leading-none">
              {entry.icon}
            </span>
          )}
          <span className="block truncate">{label}</span>
          {isPlugin && <Badge variant="info">Plugin</Badge>}
        </span>
      }
      description={
        <span className="block truncate font-mono">
          {entry.slug}
          {entry.version ? ` · v${entry.version}` : ""}
        </span>
      }
      footer={
        <>
          <span className="mr-auto text-caption text-fg-subtle">
            {anonymous ? (
              cloud ? (
                <>
                  Installing runs on a local studio pointed at your
                  workspace — {isPlugin ? "copy the CLI command" : (
                    <>download the <code className="text-fg-default">.botz</code> bundle</>
                  )} and install there.
                </>
              ) : isPlugin ? (
                <>
                  Copy the CLI command, download the source{" "}
                  <code className="text-fg-default">.zip</code>, or sign in to install.
                </>
              ) : (
                <>
                  Download the <code className="text-fg-default">.botz</code> bundle, or sign in to install.
                </>
              )
            ) : isPlugin ? (
              <>
                Installs into <code className="text-fg-default">~/.iterion/plugins/</code> — enable from Plugins.
              </>
            ) : (
              <>
                Installs into <code className="text-fg-default">.botz/</code> — never run automatically.
              </>
            )}
          </span>
          {anonymous ? (
            isPlugin ? (
              <span className="flex shrink-0 items-center gap-1.5">
                <CopyPluginInstall repoUrl={entry.repo_url} />
                <DownloadBotz slug={entry.slug} kind="plugin" />
              </span>
            ) : (
              <DownloadBotz slug={entry.slug} />
            )
          ) : (
            <InstallControls
              state={state}
              installing={installing}
              onInstall={onInstall}
              onUpdate={onUpdate}
              onUninstall={onUninstall}
            />
          )}
        </>
      }
    >
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
          <div className="max-h-96 overflow-y-auto rounded border border-border-default bg-surface-2 p-3">
            <MarkdownText value={entry.readme} size="sm" />
          </div>
        </section>
      )}
    </Drawer>
  );
}
