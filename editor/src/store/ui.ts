import { create } from "zustand";
import type { LayerKind } from "@/lib/constants";
import { TOAST_DURATION_DEFAULT_MS } from "@/lib/constants";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";

export type { LayerKind };
export type SidebarTab = "properties" | "schemas" | "prompts" | "vars" | "workflow" | "comments" | "mcp";
export type LayoutDirection = "DOWN" | "RIGHT";
export type CanvasTool = "pan" | "select";
export interface EditingItem { kind: "schema" | "prompt" | "var"; name: string }

export interface ToastAction {
  label: string;
  onClick: () => void;
}

export interface Toast {
  id: number;
  message: string;
  type: "success" | "error" | "info" | "warning";
  action?: ToastAction;
  persistent?: boolean;
}

let toastIdCounter = 0;

const INSPECTOR_WIDTH_KEY = "iterion.inspectorWidth";
const INSPECTOR_WIDTH_DEFAULT = 360;
const INSPECTOR_WIDTH_MIN = 280;
const INSPECTOR_WIDTH_MAX = 600;
const INSPECTOR_COLLAPSED_KEY = "iterion.inspectorCollapsed";

function readInspectorWidth(): number {
  if (typeof window === "undefined") return INSPECTOR_WIDTH_DEFAULT;
  const raw = window.localStorage.getItem(INSPECTOR_WIDTH_KEY);
  const parsed = raw ? parseInt(raw, 10) : NaN;
  if (!Number.isFinite(parsed)) return INSPECTOR_WIDTH_DEFAULT;
  return Math.min(INSPECTOR_WIDTH_MAX, Math.max(INSPECTOR_WIDTH_MIN, parsed));
}

interface UIState {
  activeTab: SidebarTab;
  sourceViewOpen: boolean;
  diagnosticsPanelOpen: boolean;
  expanded: boolean;
  browserFullscreen: boolean;
  activeWorkflowName: string | null;
  layoutDirection: LayoutDirection;
  activeLayers: Set<LayerKind>;
  editingItem: EditingItem | null;
  toasts: Toast[];
  // Sub-node detail view (double-click navigation)
  subNodeViewStack: string[];
  // Library panel
  libraryExpanded: boolean;
  // Macro view (all groups collapsed)
  macroView: boolean;
  // Canvas tool mode
  canvasTool: CanvasTool;
  // Inspector width (resizable right panel)
  inspectorWidth: number;
  inspectorCollapsed: boolean;
  // File picker dialog
  filePickerOpen: boolean;
  filesChangedAt: number;
  // One-shot canvas-pan-and-zoom request: set by URL-driven navigation
  // (e.g. "Open in editor" from a run) so the Canvas can fitView on the
  // target node once the document has finished loading. Consumers must
  // clear it via setPendingFitNodeId(null) after applying.
  pendingFitNodeId: string | null;
  setActiveTab: (tab: SidebarTab) => void;
  toggleSourceView: () => void;
  toggleDiagnosticsPanel: () => void;
  openDiagnosticsPanel: () => void;
  toggleExpanded: () => void;
  setBrowserFullscreen: (value: boolean) => void;
  setActiveWorkflowName: (name: string | null) => void;
  setLayoutDirection: (dir: LayoutDirection) => void;
  toggleLayoutDirection: () => void;
  toggleLayer: (layer: LayerKind) => void;
  setEditingItem: (item: EditingItem | null) => void;
  addToast: (message: string, type: Toast["type"], opts?: { action?: ToastAction; persistent?: boolean }) => void;
  removeToast: (id: number) => void;
  // Sub-node view navigation
  pushSubNodeView: (nodeId: string) => void;
  popSubNodeView: () => void;
  clearSubNodeView: () => void;
  navigateSubNodeViewTo: (index: number) => void;
  // Library panel
  toggleLibraryPanel: () => void;
  // Macro view
  toggleMacroView: () => void;
  // Canvas tool
  setCanvasTool: (tool: CanvasTool) => void;
  // Inspector width
  setInspectorWidth: (px: number) => void;
  toggleInspectorCollapsed: () => void;
  // File picker
  setFilePickerOpen: (open: boolean) => void;
  notifyFilesChanged: () => void;
  // Pending fit (URL-driven canvas centering)
  setPendingFitNodeId: (id: string | null) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeTab: "properties",
  sourceViewOpen: false,
  diagnosticsPanelOpen: false,
  expanded: false,
  browserFullscreen: false,
  activeWorkflowName: null,
  layoutDirection: "DOWN",
  activeLayers: new Set<LayerKind>(),
  editingItem: null,
  toasts: [],
  subNodeViewStack: [],
  libraryExpanded: false,
  macroView: false,
  canvasTool: "pan",
  inspectorWidth: readInspectorWidth(),
  inspectorCollapsed: readBooleanFlag(INSPECTOR_COLLAPSED_KEY),
  filePickerOpen: false,
  filesChangedAt: 0,
  pendingFitNodeId: null,
  setActiveTab: (activeTab) => set({ activeTab }),
  toggleSourceView: () => set((s) => ({ sourceViewOpen: !s.sourceViewOpen })),
  toggleDiagnosticsPanel: () => set((s) => ({ diagnosticsPanelOpen: !s.diagnosticsPanelOpen })),
  openDiagnosticsPanel: () => set((s) => (s.diagnosticsPanelOpen ? s : { diagnosticsPanelOpen: true })),
  toggleExpanded: () => set((s) => ({ expanded: !s.expanded })),
  setBrowserFullscreen: (value) => set({ browserFullscreen: value }),
  setActiveWorkflowName: (activeWorkflowName) => set({ activeWorkflowName }),
  setLayoutDirection: (layoutDirection) => set({ layoutDirection }),
  toggleLayoutDirection: () => set((s) => ({ layoutDirection: s.layoutDirection === "DOWN" ? "RIGHT" : "DOWN" })),
  toggleLayer: (layer) => set((s) => {
    const next = new Set(s.activeLayers);
    if (next.has(layer)) next.delete(layer); else next.add(layer);
    return { activeLayers: next };
  }),
  setEditingItem: (editingItem) => set((s) => (s.editingItem === editingItem ? s : { editingItem })),
  addToast: (message, type, opts) => {
    const id = ++toastIdCounter;
    set((s) => {
      // Deduplicate: remove existing persistent toast with the same message
      const filtered = opts?.persistent
        ? s.toasts.filter((t) => !(t.persistent && t.message === message))
        : s.toasts;
      return { toasts: [...filtered, { id, message, type, action: opts?.action, persistent: opts?.persistent }] };
    });
    if (!opts?.persistent) {
      setTimeout(() => {
        set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
      }, TOAST_DURATION_DEFAULT_MS);
    }
  },
  removeToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
  // Sub-node view navigation
  pushSubNodeView: (nodeId) => set((s) => {
    // Prevent duplicate: ignore if already at top of stack
    if (s.subNodeViewStack.length > 0 && s.subNodeViewStack[s.subNodeViewStack.length - 1] === nodeId) {
      return s;
    }
    return { subNodeViewStack: [...s.subNodeViewStack, nodeId] };
  }),
  // Sub-node-view "exit / change scope" actions also drop the per-item
  // edit focus. Without this, the Inspector's `editingItem` mode stays
  // pinned across navigation: e.g. clicking a prompt sub-node in
  // streak_check's subview, then clicking the breadcrumb back to the
  // global view, would leave the right panel showing the prompt editor
  // until the user manually clicked "Back" — even after picking a
  // different node in the canvas. Mirrors the contract enforced by
  // selection.ts, where setSelectedNode/Edge/clearSelection clear
  // editingItem.
  popSubNodeView: () => set((s) => ({
    subNodeViewStack: s.subNodeViewStack.slice(0, -1),
    editingItem: null,
  })),
  clearSubNodeView: () => set({ subNodeViewStack: [], editingItem: null }),
  navigateSubNodeViewTo: (index) => set((s) => ({
    subNodeViewStack: s.subNodeViewStack.slice(0, index + 1),
    editingItem: null,
  })),
  // Library panel
  toggleLibraryPanel: () => set((s) => ({ libraryExpanded: !s.libraryExpanded })),
  // Macro view
  toggleMacroView: () => set((s) => ({ macroView: !s.macroView })),
  // Canvas tool
  setCanvasTool: (canvasTool) => set({ canvasTool }),
  // Inspector width
  setInspectorWidth: (px) => {
    const clamped = Math.min(INSPECTOR_WIDTH_MAX, Math.max(INSPECTOR_WIDTH_MIN, Math.round(px)));
    if (typeof window !== "undefined") {
      window.localStorage.setItem(INSPECTOR_WIDTH_KEY, String(clamped));
    }
    set({ inspectorWidth: clamped });
  },
  toggleInspectorCollapsed: () => set((s) => {
    const next = !s.inspectorCollapsed;
    writeBooleanFlag(INSPECTOR_COLLAPSED_KEY, next);
    return { inspectorCollapsed: next };
  }),
  // File picker
  setFilePickerOpen: (filePickerOpen) => set({ filePickerOpen }),
  notifyFilesChanged: () => set({ filesChangedAt: Date.now() }),
  // Pending fit
  setPendingFitNodeId: (id) => set({ pendingFitNodeId: id }),
}));
