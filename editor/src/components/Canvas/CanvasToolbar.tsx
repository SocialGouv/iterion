import {
  ArrowDownIcon,
  ArrowRightIcon,
  Crosshair2Icon,
  DotsHorizontalIcon,
  EnterFullScreenIcon,
  ExitFullScreenIcon,
  FrameIcon,
  SizeIcon,
  StackIcon,
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
  onArrange: () => void;
  onFitView: () => void;
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
  onArrange,
  onFitView,
  onFocusNode,
  onBrowserFullscreen,
  onFitViewAfterDelay,
}: Props) {
  const activeLayers = useUIStore((s) => s.activeLayers);
  const toggleLayer = useUIStore((s) => s.toggleLayer);
  const layoutDirection = useUIStore((s) => s.layoutDirection);
  const toggleLayoutDirection = useUIStore((s) => s.toggleLayoutDirection);
  const expanded = useUIStore((s) => s.expanded);
  const toggleExpanded = useUIStore((s) => s.toggleExpanded);
  const browserFullscreen = useUIStore((s) => s.browserFullscreen);
  const macroView = useUIStore((s) => s.macroView);
  const toggleMacroView = useUIStore((s) => s.toggleMacroView);
  const document = useDocumentStore((s) => s.document);
  const hasGroups = document ? parseGroups(document.comments ?? []).length > 0 : false;

  const layoutSwitchLabel =
    layoutDirection === "DOWN"
      ? "Switch to horizontal layout (left→right)"
      : "Switch to vertical layout (top→bottom)";
  const expandLabel = expanded ? "Collapse canvas" : "Expand canvas (hide chrome)";
  const fullscreenLabel = browserFullscreen ? "Exit fullscreen" : "Enter fullscreen";

  return (
    <>
      {/* Layer toggle buttons */}
      <div className="absolute top-2 left-2 z-40 flex gap-1">
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

      {/* Right-side toolbar — icon-only to leave room for the centered breadcrumb */}
      <div className="absolute top-2 right-2 z-40 flex gap-1">
        <IconButton
          size="sm"
          variant="secondary"
          label={layoutSwitchLabel}
          onClick={() => {
            toggleLayoutDirection();
            onFitViewAfterDelay();
          }}
          className="bg-surface-1/90 border-border-strong"
        >
          {layoutDirection === "DOWN" ? <ArrowRightIcon /> : <ArrowDownIcon />}
        </IconButton>
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
            <MenuItem icon={<StackIcon />} label="Arrange" onSelect={onArrange} />
            <MenuItem icon={<FrameIcon />} label="Fit view" onSelect={onFitView} />
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
