import { useCallback, type KeyboardEvent } from "react";
import { useDocumentStore, useDocumentStoreInstance } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { makeEdgeId, isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { useEscapeStack } from "@/hooks/useEscapeStack";
import type { LayerKind } from "@/lib/constants";

// Centralised handler for the "successfully duplicated / pasted a node"
// flow: select it, schedule a fit-view animation, toast the user. The
// canvas's pendingFitNodeId effect drives the visual landing, which
// resolves the "where did my clone go?" friction without forcing the
// document store to track positions itself.
function announceNewNode(
  newName: string,
  kind: "duplicated" | "pasted",
  deps: {
    setSelectedNode: (id: string | null) => void;
    setPendingFitNodeId: (id: string | null) => void;
    addToast: (msg: string, tone: "info" | "success") => void;
  },
) {
  deps.setSelectedNode(newName);
  deps.setPendingFitNodeId(newName);
  deps.addToast(`${kind === "duplicated" ? "Duplicated" : "Pasted"} as ${newName}`, "success");
}

function isEditableNode(id: string): boolean {
  return id !== "__start__" && id !== "done" && id !== "fail" && !isAuxiliaryNodeId(id);
}

interface CanvasKeyboardDeps {
  search: {
    searchOpen: boolean;
    openSearch: () => void;
    closeSearch: () => void;
  };
  quickAddMenu: unknown;
  setQuickAddMenu: (v: null) => void;
  setContextMenu: (v: null) => void;
  // Callback the Cmd+A handler invokes with the ids of every editable
  // node. Passed in so the hook doesn't have to reach for xyflow
  // selection internals.
  onSelectAll?: (ids: string[]) => void;
}

export function useCanvasKeyboard(deps: CanvasKeyboardDeps): (e: KeyboardEvent) => void {
  const docStore = useDocumentStoreInstance();
  const document = useDocumentStore((s) => s.document);
  const removeNode = useDocumentStore((s) => s.removeNode);
  const removeEdge = useDocumentStore((s) => s.removeEdge);
  const duplicateNode = useDocumentStore((s) => s.duplicateNode);
  const undo = useDocumentStore((s) => s.undo);
  const redo = useDocumentStore((s) => s.redo);
  const selectedNodeId = useSelectionStore((s) => s.selectedNodeId);
  const selectedEdgeId = useSelectionStore((s) => s.selectedEdgeId);
  const clearSelection = useSelectionStore((s) => s.clearSelection);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);
  const copiedNodeId = useSelectionStore((s) => s.copiedNodeId);
  const setCopiedNode = useSelectionStore((s) => s.setCopiedNode);
  const addToast = useUIStore((s) => s.addToast);
  const expanded = useUIStore((s) => s.expanded);
  const toggleExpanded = useUIStore((s) => s.toggleExpanded);
  const toggleLayer = useUIStore((s) => s.toggleLayer);
  const setCanvasTool = useUIStore((s) => s.setCanvasTool);
  const setPendingFitNodeId = useUIStore((s) => s.setPendingFitNodeId);
  const dismissEscape = useEscapeStack();

  const { search, quickAddMenu, setQuickAddMenu, setContextMenu, onSelectAll } = deps;

  return useCallback(
    (e: KeyboardEvent) => {
      const isInput = (e.target as HTMLElement).matches("input, textarea, select");

      if (e.key === "/" && !isInput) {
        e.preventDefault();
        search.openSearch();
        return;
      }
      if (e.key === "Escape") {
        // Priority: close modals/sub-views first, then general UI
        if (dismissEscape()) return;
        if (expanded) { toggleExpanded(); return; }
        if (search.searchOpen) { search.closeSearch(); return; }
        if (quickAddMenu) { setQuickAddMenu(null); return; }
        clearSelection();
        setContextMenu(null);
        return;
      }

      // Layer toggle shortcuts: Alt+1=Schemas, Alt+2=Prompts, Alt+3=Vars
      if (e.altKey && !isInput && (e.key === "1" || e.key === "2" || e.key === "3")) {
        e.preventDefault();
        const layers: LayerKind[] = ["schemas", "prompts", "vars"];
        toggleLayer(layers[parseInt(e.key) - 1]!);
        return;
      }

      // Undo/Redo. We sample canUndo/canRedo before firing so the toast
      // reflects what actually happened: silently dropping the
      // shortcut when there's nothing to undo would surprise a user
      // hammering Ctrl+Z to back out of a copy/paste.
      if ((e.ctrlKey || e.metaKey) && e.key === "z" && !e.shiftKey && !isInput) {
        e.preventDefault();
        if (docStore.getState().canUndo()) {
          undo();
          addToast("Undid last change", "info");
        } else {
          addToast("Nothing to undo", "info");
        }
        return;
      }
      if ((e.ctrlKey || e.metaKey) && (e.key === "y" || (e.key === "z" && e.shiftKey)) && !isInput) {
        e.preventDefault();
        if (docStore.getState().canRedo()) {
          redo();
          addToast("Redid last change", "info");
        } else {
          addToast("Nothing to redo", "info");
        }
        return;
      }

      // Select-all on the canvas. xyflow tracks multi-selection
      // natively via the `selected` flag on each node; the parent's
      // onSelectAll callback is the bridge to its useCanvasLayout
      // selectNodes action. Excludes terminal / auxiliary nodes since
      // they're not user-editable anyway.
      if ((e.ctrlKey || e.metaKey) && (e.key === "a" || e.key === "A") && !isInput) {
        e.preventDefault();
        if (!onSelectAll) return;
        const allEditable = (document?.agents ?? [])
          .map((a) => a.name)
          .concat((document?.judges ?? []).map((j) => j.name))
          .concat((document?.routers ?? []).map((r) => r.name))
          .concat((document?.humans ?? []).map((h) => h.name))
          .concat((document?.tools ?? []).map((t) => t.name))
          .concat((document?.computes ?? []).map((c) => c.name));
        if (allEditable.length === 0) return;
        onSelectAll(allEditable);
        addToast(`Selected ${allEditable.length} node${allEditable.length === 1 ? "" : "s"}`, "info");
        return;
      }

      // Copy/Paste/Duplicate
      if ((e.ctrlKey || e.metaKey) && e.key === "c" && !isInput) {
        if (selectedNodeId && isEditableNode(selectedNodeId)) {
          setCopiedNode(selectedNodeId);
          addToast("Node copied", "info");
        }
        return;
      }
      if ((e.ctrlKey || e.metaKey) && e.key === "v" && !isInput) {
        if (copiedNodeId) {
          const newName = duplicateNode(copiedNodeId);
          if (newName) {
            announceNewNode(newName, "pasted", {
              setSelectedNode,
              setPendingFitNodeId,
              addToast,
            });
          }
        }
        return;
      }
      if ((e.ctrlKey || e.metaKey) && e.key === "d" && !isInput) {
        e.preventDefault();
        if (selectedNodeId && isEditableNode(selectedNodeId)) {
          const newName = duplicateNode(selectedNodeId);
          if (newName) {
            announceNewNode(newName, "duplicated", {
              setSelectedNode,
              setPendingFitNodeId,
              addToast,
            });
          }
        }
        return;
      }

      // Tool switching shortcuts
      if ((e.key === "v" || e.key === "V") && !isInput && !e.ctrlKey && !e.metaKey) {
        setCanvasTool("select");
        return;
      }
      if ((e.key === "h" || e.key === "H") && !isInput && !e.ctrlKey && !e.metaKey) {
        setCanvasTool("pan");
        return;
      }

      if (e.key === "Delete" || e.key === "Backspace") {
        if (isInput) return;

        if (selectedNodeId && isEditableNode(selectedNodeId)) {
          removeNode(selectedNodeId);
          clearSelection();
        } else if (selectedEdgeId && document) {
          for (const wf of document.workflows) {
            const wfEdges = wf.edges ?? [];
            for (let i = 0; i < wfEdges.length; i++) {
              const id = makeEdgeId(wf.name, i);
              if (id === selectedEdgeId) {
                removeEdge(wf.name, i);
                clearSelection();
                return;
              }
            }
          }
        }
      }
    },
    [selectedNodeId, selectedEdgeId, document, removeNode, removeEdge, clearSelection, search, quickAddMenu, copiedNodeId, duplicateNode, setCopiedNode, setSelectedNode, addToast, expanded, toggleExpanded, dismissEscape, toggleLayer, undo, redo, setQuickAddMenu, setContextMenu, setCanvasTool, onSelectAll, setPendingFitNodeId],
  );
}
