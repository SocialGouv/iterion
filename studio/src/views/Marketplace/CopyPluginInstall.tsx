import { CopyButton } from "@/components/ui/CopyButton";

/** CopyPluginInstall is shown in place of the `.botz` download for
 *  plugin entries when the viewer has no workspace to install into
 *  (anonymous / cloud): plugins have no packed bundle artifact (the
 *  download endpoint 400s), so the takeaway is the CLI command that
 *  installs the plugin locally. Stops click propagation so the enclosing
 *  card's open-on-click doesn't also fire. */
export function CopyPluginInstall({ repoUrl }: { repoUrl: string }) {
  const cmd = `iterion plugin install ${repoUrl}`;
  return (
    <span
      onClick={(e) => e.stopPropagation()}
      className="inline-flex min-w-0 shrink-0 items-center gap-1 rounded border border-border-default bg-surface-1 px-2 py-1"
    >
      <code className="max-w-[16rem] truncate font-mono text-caption text-fg-muted" title={cmd}>
        {cmd}
      </code>
      <CopyButton value={cmd} variant="icon" label="Copy install command" />
    </span>
  );
}
