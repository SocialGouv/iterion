import { create } from "zustand";
import type {
  IterDocument,
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  JoinDecl,
  HumanDecl,
  ToolNodeDecl,
  WorkflowDecl,
  SchemaDecl,
  PromptDecl,
  VarsBlock,
  BudgetBlock,
  Edge,
} from "@/api/types";

// Normalize a document from JSON (omitempty may leave arrays as undefined).
function normalize(doc: IterDocument): IterDocument {
  return {
    ...doc,
    prompts: doc.prompts ?? [],
    schemas: doc.schemas ?? [],
    agents: doc.agents ?? [],
    judges: doc.judges ?? [],
    routers: doc.routers ?? [],
    joins: doc.joins ?? [],
    humans: doc.humans ?? [],
    tools: doc.tools ?? [],
    workflows: (doc.workflows ?? []).map((w) => ({
      ...w,
      edges: w.edges ?? [],
    })),
    comments: doc.comments ?? [],
  };
}

interface DocumentState {
  document: IterDocument | null;
  diagnostics: string[];
  warnings: string[];

  // Document lifecycle
  setDocument: (doc: IterDocument) => void;
  setDiagnostics: (d: string[], w?: string[]) => void;

  // Node updates
  updateAgent: (name: string, updates: Partial<AgentDecl>) => void;
  updateJudge: (name: string, updates: Partial<JudgeDecl>) => void;
  updateRouter: (name: string, updates: Partial<RouterDecl>) => void;
  updateJoin: (name: string, updates: Partial<JoinDecl>) => void;
  updateHuman: (name: string, updates: Partial<HumanDecl>) => void;
  updateTool: (name: string, updates: Partial<ToolNodeDecl>) => void;
  updateWorkflow: (name: string, updates: Partial<WorkflowDecl>) => void;

  // Node add/remove
  addAgent: (decl: AgentDecl) => void;
  addJudge: (decl: JudgeDecl) => void;
  addRouter: (decl: RouterDecl) => void;
  addJoin: (decl: JoinDecl) => void;
  addHuman: (decl: HumanDecl) => void;
  addTool: (decl: ToolNodeDecl) => void;
  removeNode: (name: string) => void;
  renameNode: (oldName: string, newName: string) => void;

  // Edge mutations
  addEdge: (workflowName: string, edge: Edge) => void;
  removeEdge: (workflowName: string, edgeIndex: number) => void;
  updateEdge: (workflowName: string, edgeIndex: number, updates: Partial<Edge>) => void;

  // Schema mutations
  addSchema: (decl: SchemaDecl) => void;
  removeSchema: (name: string) => void;
  updateSchema: (name: string, updates: Partial<SchemaDecl>) => void;

  // Prompt mutations
  addPrompt: (decl: PromptDecl) => void;
  removePrompt: (name: string) => void;
  updatePrompt: (name: string, updates: Partial<PromptDecl>) => void;

  // Vars mutations
  setVars: (vars: VarsBlock | undefined) => void;
  setWorkflowVars: (workflowName: string, vars: VarsBlock | undefined) => void;

  // Budget mutations
  updateWorkflowBudget: (workflowName: string, budget: BudgetBlock | undefined) => void;
}

function updateInArray<T extends { name: string }>(arr: T[], name: string, updates: Partial<T>): T[] {
  return arr.map((item) => (item.name === name ? { ...item, ...updates } : item));
}

function updateWorkflowsEdges(doc: IterDocument, oldName: string, newName: string): WorkflowDecl[] {
  return doc.workflows.map((w) => ({
    ...w,
    entry: w.entry === oldName ? newName : w.entry,
    edges: w.edges.map((e) => ({
      ...e,
      from: e.from === oldName ? newName : e.from,
      to: e.to === oldName ? newName : e.to,
    })),
  }));
}

function removeNodeEdges(doc: IterDocument, name: string): WorkflowDecl[] {
  return doc.workflows.map((w) => ({
    ...w,
    entry: w.entry === name ? "" : w.entry,
    edges: w.edges.filter((e) => e.from !== name && e.to !== name),
  }));
}

export const useDocumentStore = create<DocumentState>((set) => ({
  document: null,
  diagnostics: [],
  warnings: [],

  setDocument: (document) => set({ document: normalize(document) }),
  setDiagnostics: (diagnostics, warnings = []) => set({ diagnostics: diagnostics ?? [], warnings: warnings ?? [] }),

  // Node updates
  updateAgent: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, agents: updateInArray(s.document.agents, name, updates) } } : s)),
  updateJudge: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, judges: updateInArray(s.document.judges, name, updates) } } : s)),
  updateRouter: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, routers: updateInArray(s.document.routers, name, updates) } } : s)),
  updateJoin: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, joins: updateInArray(s.document.joins, name, updates) } } : s)),
  updateHuman: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, humans: updateInArray(s.document.humans, name, updates) } } : s)),
  updateTool: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, tools: updateInArray(s.document.tools, name, updates) } } : s)),
  updateWorkflow: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: updateInArray(s.document.workflows, name, updates) } } : s)),

  // Node add
  addAgent: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, agents: [...s.document.agents, decl] } } : s)),
  addJudge: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, judges: [...s.document.judges, decl] } } : s)),
  addRouter: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, routers: [...s.document.routers, decl] } } : s)),
  addJoin: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, joins: [...s.document.joins, decl] } } : s)),
  addHuman: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, humans: [...s.document.humans, decl] } } : s)),
  addTool: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, tools: [...s.document.tools, decl] } } : s)),

  // Node remove — removes declaration + cleans up all edges referencing it + cleans join.require[]
  removeNode: (name) =>
    set((s) => {
      if (!s.document) return s;
      const doc = s.document;
      return {
        document: {
          ...doc,
          agents: doc.agents.filter((a) => a.name !== name),
          judges: doc.judges.filter((j) => j.name !== name),
          routers: doc.routers.filter((r) => r.name !== name),
          joins: doc.joins
            .filter((j) => j.name !== name)
            .map((j) => ({ ...j, require: j.require.filter((r) => r !== name) })),
          humans: doc.humans.filter((h) => h.name !== name),
          tools: doc.tools.filter((t) => t.name !== name),
          workflows: removeNodeEdges(doc, name),
        },
      };
    }),

  // Node rename — updates all references
  renameNode: (oldName, newName) =>
    set((s) => {
      if (!s.document || oldName === newName) return s;
      const doc = s.document;
      const renameIn = <T extends { name: string }>(arr: T[]) =>
        arr.map((item) => (item.name === oldName ? { ...item, name: newName } : item));
      return {
        document: {
          ...doc,
          agents: renameIn(doc.agents),
          judges: renameIn(doc.judges),
          routers: renameIn(doc.routers),
          joins: renameIn(doc.joins).map((j) => ({
            ...j,
            require: j.require.map((r) => (r === oldName ? newName : r)),
          })),
          humans: renameIn(doc.humans),
          tools: renameIn(doc.tools),
          workflows: updateWorkflowsEdges(doc, oldName, newName),
        },
      };
    }),

  // Edge mutations
  addEdge: (workflowName, edge) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) =>
            w.name === workflowName ? { ...w, edges: [...w.edges, edge] } : w,
          ),
        },
      };
    }),

  removeEdge: (workflowName, edgeIndex) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) =>
            w.name === workflowName ? { ...w, edges: w.edges.filter((_, i) => i !== edgeIndex) } : w,
          ),
        },
      };
    }),

  updateEdge: (workflowName, edgeIndex, updates) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) =>
            w.name === workflowName
              ? { ...w, edges: w.edges.map((e, i) => (i === edgeIndex ? { ...e, ...updates } : e)) }
              : w,
          ),
        },
      };
    }),

  // Schema mutations
  addSchema: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: [...s.document.schemas, decl] } } : s)),
  removeSchema: (name) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: s.document.schemas.filter((d) => d.name !== name) } } : s)),
  updateSchema: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: updateInArray(s.document.schemas, name, updates) } } : s)),

  // Prompt mutations
  addPrompt: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: [...s.document.prompts, decl] } } : s)),
  removePrompt: (name) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: s.document.prompts.filter((d) => d.name !== name) } } : s)),
  updatePrompt: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: updateInArray(s.document.prompts, name, updates) } } : s)),

  // Vars mutations
  setVars: (vars) =>
    set((s) => (s.document ? { document: { ...s.document, vars } } : s)),
  setWorkflowVars: (workflowName, vars) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) => (w.name === workflowName ? { ...w, vars } : w)),
        },
      };
    }),

  // Budget mutations
  updateWorkflowBudget: (workflowName, budget) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) => (w.name === workflowName ? { ...w, budget } : w)),
        },
      };
    }),
}));
