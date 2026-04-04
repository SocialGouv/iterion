import type { LayerKind } from "@/store/ui";

const LAYER_TOGGLES: { kind: LayerKind; label: string; icon: string }[] = [
  { kind: "schemas", label: "Schemas", icon: "\u{1F4D0}" },
  { kind: "prompts", label: "Prompts", icon: "\u{1F4DD}" },
  { kind: "vars", label: "Vars", icon: "\u{1F3F7}\u{FE0F}" },
];

interface Props {
  activeLayers: Set<LayerKind>;
  toggleLayer: (layer: LayerKind) => void;
  layoutDirection: "DOWN" | "RIGHT";
  toggleLayoutDirection: () => void;
  onArrange: () => void;
  onFitView: () => void;
  onFocusNode: (() => void) | null;
  expanded: boolean;
  toggleExpanded: () => void;
  browserFullscreen: boolean;
  onBrowserFullscreen: () => void;
  onFitViewAfterDelay: () => void;
}

export default function CanvasToolbar({
  activeLayers,
  toggleLayer,
  layoutDirection,
  toggleLayoutDirection,
  onArrange,
  onFitView,
  onFocusNode,
  expanded,
  toggleExpanded,
  browserFullscreen,
  onBrowserFullscreen,
  onFitViewAfterDelay,
}: Props) {
  return (
    <>
      {/* Layer toggle buttons */}
      <div className="absolute top-2 left-2 z-40 flex gap-1">
        {LAYER_TOGGLES.map(({ kind, label, icon }) => (
          <button
            key={kind}
            className={`border text-xs px-2 py-1 rounded flex items-center gap-1 ${
              activeLayers.has(kind)
                ? "bg-blue-600 hover:bg-blue-700 border-blue-500 text-white"
                : "bg-gray-800/90 hover:bg-gray-700 border-gray-600 text-gray-300"
            }`}
            onClick={() => toggleLayer(kind)}
            title={`Toggle ${label} layer (Alt+${kind === "schemas" ? "1" : kind === "prompts" ? "2" : "3"})`}
          >
            <span>{icon}</span>
            {label}
          </button>
        ))}
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
