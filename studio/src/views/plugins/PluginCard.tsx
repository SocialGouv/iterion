import { CheckIcon } from "@radix-ui/react-icons";
import {
  BookOpen,
  Bot,
  Puzzle,
  RefreshCw,
  Server,
  SquareTerminal,
  Webhook,
  Zap,
} from "lucide-react";

import type { PluginView } from "@/api/plugins";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";

interface Props {
  plugin: PluginView;
  busy: boolean;
  // Enable/disable rewrites the host-global plugins.yaml — on a shared
  // cloud server only super-admins may flip it; others see a read-only
  // state chip.
  canManage: boolean;
  onEnable: () => void;
  onOpen: () => void;
}

// kindIcon maps a plugin's primary contribution kind to a neutral glyph —
// plugins have no emoji/logo, so the icon slot reads the manifest's first
// declared kind (rewriter, mcp, skill, …) and falls back to a puzzle piece.
function kindIcon(kinds: string[]) {
  const cls = "h-4 w-4";
  switch (kinds[0]) {
    case "rewriter":
      return <Zap className={cls} aria-hidden />;
    case "mcp":
      return <Server className={cls} aria-hidden />;
    case "skill":
      return <BookOpen className={cls} aria-hidden />;
    case "command":
      return <SquareTerminal className={cls} aria-hidden />;
    case "agent":
      return <Bot className={cls} aria-hidden />;
    case "hook":
      return <Webhook className={cls} aria-hidden />;
    case "lifecycle":
      return <RefreshCw className={cls} aria-hidden />;
    default:
      return <Puzzle className={cls} aria-hidden />;
  }
}

/** PluginCard renders one registry plugin as a compact clickable tile
 *  (mirrors MarketplaceCard's card tokens). The whole card opens the
 *  detail drawer; the footer's single action — Enable, or the green
 *  "Enabled" chip — stops propagation so it doesn't double-trigger. */
export default function PluginCard({ plugin: p, busy, canManage, onEnable, onOpen }: Props) {
  return (
    <li
      onClick={onOpen}
      className="flex h-full cursor-pointer flex-col gap-2 rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 shadow-[var(--shadow-sm)] transition-[box-shadow,border-color,transform] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)] focus-within:border-border-strong"
    >
      {/* Keyboard path: the content is a real button (Enter/Space); its click
          bubbles to the <li>'s onOpen, so there's a single open handler. */}
      <button
        type="button"
        className="flex flex-1 flex-col items-start gap-1.5 rounded text-left focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
      >
        <div className="flex w-full items-center gap-2">
          <span
            aria-hidden
            className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md border border-border-default bg-surface-2 text-fg-subtle"
          >
            {kindIcon(p.kinds)}
          </span>
          <div className="flex min-w-0 flex-1 items-baseline gap-2">
            <span className="truncate font-mono text-sm text-fg-default">{p.name}</span>
            {p.version && (
              <span className="shrink-0 text-caption text-fg-subtle">{p.version}</span>
            )}
          </div>
          {p.builtin && (
            <Badge variant="info" size="sm" className="shrink-0">
              builtin
            </Badge>
          )}
        </div>
        {p.description && (
          <p className="line-clamp-2 text-xs text-fg-muted">{p.description}</p>
        )}
        {p.kinds.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {p.kinds.map((k) => (
              <Badge key={k} variant="accent" size="sm">
                {k}
              </Badge>
            ))}
          </div>
        )}
      </button>
      <div className="flex items-center justify-end gap-2">
        {p.enabled ? (
          // Same chip pattern as the marketplace's InstallControls "Installed"
          // state — disable lives in the detail drawer only.
          <span className="inline-flex items-center gap-1 rounded bg-surface-1 px-1.5 py-0.5 text-caption text-success-fg">
            <CheckIcon className="h-3 w-3" /> Enabled
          </span>
        ) : canManage ? (
          <Button
            variant="primary"
            size="sm"
            loading={busy}
            disabled={busy}
            className="shrink-0"
            onClick={(e) => {
              e.stopPropagation();
              onEnable();
            }}
          >
            Enable
          </Button>
        ) : (
          <span className="text-caption text-fg-subtle">Disabled by admin</span>
        )}
      </div>
    </li>
  );
}
