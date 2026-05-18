import {
  Crosshair2Icon,
  DotsHorizontalIcon,
  EnterFullScreenIcon,
  ExitFullScreenIcon,
  SizeIcon,
} from "@radix-ui/react-icons";
import type { ReactNode } from "react";

import { useUIStore } from "@/store/ui";
import { useDocumentStore } from "@/store/document";
import { IconButton, Popover, PopoverClose } from "@/components/ui";
import { LAYER_ICONS, LAYER_LABELS } from "@/lib/constants";
import type { LayerKind } from "@/lib/constants";
import { parseGroups } from "@/lib/groups";

const LAYER_KINDS: LayerKind[] = ["schemas", "prompts", "vars"];

interface Props {
  onFocusNode: (() => void) | null;
  onBrowserFullscreen: () => void;
  onFitViewAfterDelay: () => void;
}

interface MenuItemProps {
  icon: ReactNode;
  label: string;
  onSelect: () => void;
}

function MenuItem({ icon, label, onSelect }: MenuItemProps) {
  return (
    <PopoverClose asChild>
      <button
        type="button"
        onClick={onSelect}
        className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-xs text-fg-default hover:bg-surface-2 focus:outline-none focus:bg-surface-2"
      >
        <span className="text-fg-muted">{icon}</span>
        <span>{label}</span>
      </button>
    </PopoverClose>
  );
}

export default function CanvasToolbar({
  onFocusNode,
  onBrowserFullscreen,
  onFitViewAfterDelay,
}: Props) {
  const activeLayers = useUIStore((s) => s.activeLayers);
  const toggleLayer = useUIStore((s) => s.toggleLayer);
  const expanded = useUIStore((s) => s.expanded);
  const toggleExpanded = useUIStore((s) => s.toggleExpanded);
  const browserFullscreen = useUIStore((s) => s.browserFullscreen);
  const macroView = useUIStore((s) => s.macroView);
  const toggleMacroView = useUIStore((s) => s.toggleMacroView);
  const document = useDocumentStore((s) => s.document);
  const hasGroups = document ? parseGroups(document.comments ?? []).length > 0 : false;

  const expandLabel = expanded ? "Collapse canvas" : "Expand canvas (hide chrome)";
  const fullscreenLabel = browserFullscreen ? "Exit fullscreen" : "Enter fullscreen";

  return (
    <>
      {/* Layer toggle buttons */}
      <div className="absolute top-2 left-2 z-[var(--z-canvas)] flex gap-1">
        {LAYER_KINDS.map((kind, i) => (
          <button
            key={kind}
            className={`border text-xs px-2 py-1 rounded flex items-center gap-1 ${
              activeLayers.has(kind)
                ? "bg-accent hover:bg-accent-hover border-accent text-fg-default"
                : "bg-surface-1/90 hover:bg-surface-2 border-border-strong text-fg-muted"
            }`}
            onClick={() => toggleLayer(kind)}
            title={`Toggle ${LAYER_LABELS[kind]} layer (Alt+${i + 1})`}
          >
            <span>{LAYER_ICONS[kind]}</span>
            {LAYER_LABELS[kind]}
          </button>
        ))}
        {hasGroups && (
          <button
            className={`border text-xs px-2 py-1 rounded flex items-center gap-1 ${
              macroView
                ? "bg-accent hover:bg-accent border-accent text-fg-default"
                : "bg-surface-1/90 hover:bg-surface-2 border-border-strong text-fg-muted"
            }`}
            onClick={() => { toggleMacroView(); onFitViewAfterDelay(); }}
            title="Toggle macro view (show groups as nodes)"
          >
            <span>{"\u{1F4E6}"}</span>
            Macro
          </button>
        )}
      </div>

      {/* Right-side toolbar — situational canvas actions only. Layout
          direction, Arrange and Fit-view live in the top Toolbar's View
          group so they're one click away regardless of canvas focus. */}
      <div className="absolute top-2 right-2 z-[var(--z-canvas)] flex gap-1">
        <Popover
          side="bottom"
          align="end"
          contentClassName="p-1 min-w-[180px]"
          trigger={
            <IconButton
              size="sm"
              variant="secondary"
              label="More canvas actions"
              className="bg-surface-1/90 border-border-strong"
            >
              <DotsHorizontalIcon />
            </IconButton>
          }
        >
          <div className="flex flex-col">
            {onFocusNode && (
              <MenuItem
                icon={<Crosshair2Icon />}
                label="Focus selected"
                onSelect={onFocusNode}
              />
            )}
            <MenuItem
              icon={<SizeIcon />}
              label={expandLabel}
              onSelect={() => {
                toggleExpanded();
                onFitViewAfterDelay();
              }}
            />
            <MenuItem
              icon={browserFullscreen ? <ExitFullScreenIcon /> : <EnterFullScreenIcon />}
              label={fullscreenLabel}
              onSelect={() => {
                onBrowserFullscreen();
                onFitViewAfterDelay();
              }}
            />
          </div>
        </Popover>
      </div>
    </>
  );
}
