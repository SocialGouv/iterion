import type { IterDocument } from "@/api/types";

/** Valid relation types for subnode assignments. */
export type SubNodeRelation = "input" | "output" | "system" | "user" | "instructions";

/**
 * Sets a field on the node matching nodeId across all node-type arrays.
 * Since names are unique, only one array element will actually change.
 */
export function assignFieldToNode(doc: IterDocument, nodeId: string, field: string, value: string): IterDocument {
  return {
    ...doc,
    agents: doc.agents.map((a) => (a.name === nodeId ? { ...a, [field]: value } : a)),
    judges: doc.judges.map((j) => (j.name === nodeId ? { ...j, [field]: value } : j)),
    routers: doc.routers.map((r) => (r.name === nodeId ? { ...r, [field]: value } : r)),
    humans: doc.humans.map((h) => (h.name === nodeId ? { ...h, [field]: value } : h)),
    tools: doc.tools.map((t) => (t.name === nodeId ? { ...t, [field]: value } : t)),
  };
}

/**
 * Adds a tool name to an agent/judge's tools array (no-op if already present).
 */
export function addToolToNode(doc: IterDocument, nodeId: string, toolName: string): IterDocument {
  return {
    ...doc,
    agents: doc.agents.map((a) =>
      a.name === nodeId && !(a.tools ?? []).includes(toolName)
        ? { ...a, tools: [...(a.tools ?? []), toolName] }
        : a,
    ),
    judges: doc.judges.map((j) =>
      j.name === nodeId && !(j.tools ?? []).includes(toolName)
        ? { ...j, tools: [...(j.tools ?? []), toolName] }
        : j,
    ),
  };
}
