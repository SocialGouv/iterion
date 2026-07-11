import { DownloadIcon } from "@radix-ui/react-icons";

import { marketplaceDownloadUrl } from "@/api/marketplace";

/** DownloadBotz is the public, no-auth download action for marketplace
 *  visitors: the entry's archive — a packed `.botz` bundle for bots, the
 *  plugin's source tree as a `.zip` for plugins. Rendered as a styled
 *  anchor (not a <Button>, which is a <button> — nesting one in an <a> is
 *  invalid) so the browser handles the download natively. Stops click
 *  propagation so the enclosing card's open-on-click doesn't also fire. */
export function DownloadBotz({ slug, kind = "bot" }: { slug: string; kind?: "bot" | "plugin" }) {
  return (
    <a
      href={marketplaceDownloadUrl(slug)}
      download
      onClick={(e) => e.stopPropagation()}
      className="inline-flex shrink-0 items-center gap-1 rounded border border-border-default bg-surface-1 px-2.5 py-1 text-xs font-medium text-fg-default transition-colors hover:bg-surface-2 focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
    >
      <DownloadIcon className="h-3 w-3" /> {kind === "plugin" ? ".zip" : ".botz"}
    </a>
  );
}
