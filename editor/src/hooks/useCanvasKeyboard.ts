import { useCallback, type KeyboardEvent } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import { makeEdgeId, isAuxiliaryNodeId } from "@/lib/documentToGraph";
import { useEscapeStack } from "@/hooks/useEscapeStack";
import type { LayerKind } from "@/lib/constants";

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
}

export function useCanvasKeyboard(deps: CanvasKeyboardDeps): (e: KeyboardEvent) => void {
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
  const dismissEscape = useEscapeStack();

  const { search, quickAddMenu, setQuickAddMenu, setContextMenu } = deps;

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

      // Undo/Redo
      if ((e.ctrlKey || e.metaKey) && e.key === "z" && !e.shiftKey && !isInput) {
        e.preventDefault();
        undo();
        return;
      }
      if ((e.ctrlKey || e.metaKey) && (e.key === "y" || (e.key === "z" && e.shiftKey)) && !isInput) {
        e.preventDefault();
        redo();
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
            setSelectedNode(newName);
            addToast(`Pasted as ${newName}`, "success");
          }
        }
        return;
      }
      if ((e.ctrlKey || e.metaKey) && e.key === "d" && !isInput) {
        e.preventDefault();
        if (selectedNodeId && isEditableNode(selectedNodeId)) {
          const newName = duplicateNode(selectedNodeId);
          if (newName) {
            setSelectedNode(newName);
            addToast(`Duplicated as ${newName}`, "success");
          }
        }
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
    [selectedNodeId, selectedEdgeId, document, removeNode, removeEdge, clearSelection, search, quickAddMenu, copiedNodeId, duplicateNode, setCopiedNode, setSelectedNode, addToast, expanded, toggleExpanded, dismissEscape, toggleLayer, undo, redo, setQuickAddMenu, setContextMenu],
  );
}
