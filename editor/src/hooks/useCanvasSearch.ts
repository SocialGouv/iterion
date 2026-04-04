import { useCallback, useMemo, useRef, useState } from "react";
import type { Node } from "@xyflow/react";
import { useSelectionStore } from "@/store/selection";

interface CanvasSearchResult {
  searchOpen: boolean;
  searchQuery: string;
  matchedNodeIds: string[];
  currentMatchIndex: number;
  searchInputRef: React.RefObject<HTMLInputElement | null>;
  openSearch: () => void;
  closeSearch: () => void;
  setSearchQuery: (q: string) => void;
  nextMatch: () => void;
  prevMatch: () => void;
  selectCurrentMatch: () => void;
  /** Apply search dimming/highlighting to nodes */
  applySearchFilter: (nodes: Node[]) => Node[];
}

function nodeMatchesQuery(node: Node, q: string): boolean {
  const data = node.data as { label?: string; kind?: string } | undefined;
  return (
    node.id.toLowerCase().includes(q) ||
    (data?.label ?? "").toLowerCase().includes(q) ||
    (data?.kind ?? "").toLowerCase().includes(q)
  );
}

export function useCanvasSearch(layoutNodes: Node[]): CanvasSearchResult {
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [currentMatchIndex, setCurrentMatchIndex] = useState(0);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const matchedNodeIds = useMemo(() => {
    if (!searchOpen || !searchQuery.trim()) return [];
    const q = searchQuery.trim().toLowerCase();
    return layoutNodes
      .filter((n) => nodeMatchesQuery(n, q))
      .map((n) => n.id);
  }, [layoutNodes, searchOpen, searchQuery]);

  const openSearch = useCallback(() => {
    setSearchOpen(true);
    setCurrentMatchIndex(0);
    setTimeout(() => searchInputRef.current?.focus(), 0);
  }, []);

  const closeSearch = useCallback(() => {
    setSearchOpen(false);
    setSearchQuery("");
    setCurrentMatchIndex(0);
  }, []);

  const nextMatch = useCallback(() => {
    if (matchedNodeIds.length === 0) return;
    setCurrentMatchIndex((i) => (i + 1) % matchedNodeIds.length);
  }, [matchedNodeIds.length]);

  const prevMatch = useCallback(() => {
    if (matchedNodeIds.length === 0) return;
    setCurrentMatchIndex((i) => (i - 1 + matchedNodeIds.length) % matchedNodeIds.length);
  }, [matchedNodeIds.length]);

  const selectCurrentMatch = useCallback(() => {
    if (matchedNodeIds.length === 0) return;
    const id = matchedNodeIds[currentMatchIndex];
    if (id) {
      setSelectedNode(id);
      closeSearch();
    }
  }, [matchedNodeIds, currentMatchIndex, setSelectedNode, closeSearch]);

  const applySearchFilter = useCallback(
    (nodes: Node[]): Node[] => {
      if (!searchOpen || !searchQuery.trim()) return nodes;
      const currentId = matchedNodeIds[currentMatchIndex];
      return nodes.map((n) => {
        if (!matchedNodeIds.includes(n.id)) {
          return { ...n, style: { ...n.style, opacity: 0.25 } };
        }
        if (n.id === currentId) {
          return {
            ...n,
            style: {
              ...n.style,
              opacity: 1,
              boxShadow: "0 0 0 2px #60A5FA, 0 0 12px rgba(96, 165, 250, 0.4)",
              borderRadius: "8px",
            },
          };
        }
        return { ...n, style: { ...n.style, opacity: 1 } };
      });
    },
    [searchOpen, searchQuery, matchedNodeIds, currentMatchIndex],
  );

  return {
    searchOpen,
    searchQuery,
    matchedNodeIds,
    currentMatchIndex,
    searchInputRef,
    openSearch,
    closeSearch,
    setSearchQuery,
    nextMatch,
    prevMatch,
    selectCurrentMatch,
    applySearchFilter,
  };
}
