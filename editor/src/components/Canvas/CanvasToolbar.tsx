import { useUIStore } from "@/store/ui";
import { useDocumentStore } from "@/store/document";
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

  return (
    <>
      {/* Layer toggle buttons */}
      <div className="absolute top-2 left-2 z-40 flex gap-1">
        {LAYER_KINDS.map((kind, i) => (
          <button
            key={kind}
            className={`border text-xs px-2 py-1 rounded flex items-center gap-1 ${
              activeLayers.has(kind)
                ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
                : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
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
                ? "bg-indigo-600 hover:bg-indigo-700 border-indigo-500 text-white"
                : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
            }`}
            onClick={() => { toggleMacroView(); onFitViewAfterDelay(); }}
            title="Toggle macro view (show groups as nodes)"
          >
            <span>{"\u{1F4E6}"}</span>
            Macro
          </button>
        )}
      </div>

      {/* Right-side toolbar */}
      <div className="absolute top-2 right-2 z-40 flex gap-1">
        <button
          className={`border text-xs px-2 py-1 rounded ${
            layoutDirection === "RIGHT"
              ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
              : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
          }`}
          onClick={() => {
            toggleLayoutDirection();
            onFitViewAfterDelay();
          }}
          title={layoutDirection === "DOWN" ? "Switch to horizontal layout (left\u2192right)" : "Switch to vertical layout (top\u2192bottom)"}
        >
          {layoutDirection === "DOWN" ? "\u2194 Horizontal" : "\u2195 Vertical"}
        </button>
        <button
          className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
          onClick={onArrange}
          title="Auto-arrange nodes chronologically"
        >
          Arrange
        </button>
        <button
          className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
          onClick={onFitView}
          title="Fit all nodes in view"
        >
          Fit
        </button>
        {onFocusNode && (
          <button
            className="bg-gray-800/90 hover:bg-gray-700 border border-gray-600 text-xs px-2 py-1 rounded text-gray-300"
            onClick={onFocusNode}
            title="Zoom to selected node"
          >
            Focus
          </button>
        )}
        <button
          className={`border text-xs px-2 py-1 rounded ${
            expanded
              ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
              : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
          }`}
          onClick={() => { toggleExpanded(); onFitViewAfterDelay(); }}
          title={expanded ? "Collapse canvas (Esc)" : "Expand canvas (hide chrome)"}
        >
          {expanded ? "Collapse" : "Expand"}
        </button>
        <button
          className={`border text-xs px-2 py-1 rounded ${
            browserFullscreen
              ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
              : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
          }`}
          onClick={() => { onBrowserFullscreen(); onFitViewAfterDelay(); }}
          title={browserFullscreen ? "Exit fullscreen" : "Enter fullscreen"}
        >
          {browserFullscreen ? "Exit FS" : "Fullscreen"}
        </button>
      </div>
    </>
  );
}
