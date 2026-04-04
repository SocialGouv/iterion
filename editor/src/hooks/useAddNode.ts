import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { useSelectionStore } from "@/store/selection";
import { generateUniqueName, getAllNodeNames, defaultAgent, defaultJudge, defaultRouter, defaultJoin, defaultHuman, defaultTool } from "@/lib/defaults";
import type { NodeKind } from "@/api/types";

/**
 * Hook that encapsulates the pattern of creating a new node by kind:
 * generates a unique name, creates the default declaration, adds to the store, and selects it.
 *
 * Returns the new node name for further use (e.g., setting pending position, adding an edge).
 */
export function useAddNode() {
  const document = useDocumentStore((s) => s.document);
  const addAgent = useDocumentStore((s) => s.addAgent);
  const addJudge = useDocumentStore((s) => s.addJudge);
  const addRouter = useDocumentStore((s) => s.addRouter);
  const addJoin = useDocumentStore((s) => s.addJoin);
  const addHuman = useDocumentStore((s) => s.addHuman);
  const addTool = useDocumentStore((s) => s.addTool);
  const setSelectedNode = useSelectionStore((s) => s.setSelectedNode);

  const addNode = useCallback(
    (kind: NodeKind): string | null => {
      if (!document) return null;
      if (kind === "done" || kind === "fail" || kind === "start") return null;

      const existingNames = getAllNodeNames(document);
      const name = generateUniqueName(kind, existingNames);

      switch (kind) {
        case "agent": addAgent(defaultAgent(name)); break;
        case "judge": addJudge(defaultJudge(name)); break;
        case "router": addRouter(defaultRouter(name)); break;
        case "join": addJoin(defaultJoin(name)); break;
        case "human": addHuman(defaultHuman(name)); break;
        case "tool": addTool(defaultTool(name)); break;
      }

      setSelectedNode(name);
      return name;
    },
    [document, addAgent, addJudge, addRouter, addJoin, addHuman, addTool, setSelectedNode],
  );

  return addNode;
}
