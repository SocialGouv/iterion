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
  Comment,
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

const MAX_HISTORY = 50;

interface DocumentState {
  document: IterDocument | null;
  diagnostics: string[];
  warnings: string[];
  currentFilePath: string | null;
  _savedSnapshot: string | null;

  // Undo/redo
  _history: IterDocument[];
  _future: IterDocument[];

  // Document lifecycle
  setDocument: (doc: IterDocument) => void;
  setDiagnostics: (d: string[], w?: string[]) => void;
  setCurrentFilePath: (path: string | null) => void;
  markSaved: () => void;
  isDirty: () => boolean;

  // Undo/redo
  undo: () => void;
  redo: () => void;
  canUndo: () => boolean;
  canRedo: () => boolean;

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
  duplicateNode: (name: string) => string | null;

  // Workflow management
  addWorkflow: (decl: WorkflowDecl) => void;
  removeWorkflow: (name: string) => void;

  // Edge mutations
  addEdge: (workflowName: string, edge: Edge) => void;
  removeEdge: (workflowName: string, edgeIndex: number) => void;
  updateEdge: (workflowName: string, edgeIndex: number, updates: Partial<Edge>) => void;

  // Schema mutations
  addSchema: (decl: SchemaDecl) => void;
  removeSchema: (name: string) => void;
  updateSchema: (name: string, updates: Partial<SchemaDecl>) => void;
  renameSchema: (oldName: string, newName: string) => void;

  // Prompt mutations
  addPrompt: (decl: PromptDecl) => void;
  removePrompt: (name: string) => void;
  updatePrompt: (name: string, updates: Partial<PromptDecl>) => void;
  renamePrompt: (oldName: string, newName: string) => void;

  // Vars mutations
  setVars: (vars: VarsBlock | undefined) => void;
  setWorkflowVars: (workflowName: string, vars: VarsBlock | undefined) => void;

  // Budget mutations
  updateWorkflowBudget: (workflowName: string, budget: BudgetBlock | undefined) => void;

  // Comment mutations
  addComment: (comment: Comment) => void;
  removeComment: (index: number) => void;
  updateComment: (index: number, text: string) => void;
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

/** Push current document onto history before making a change. */
function pushHistory(s: DocumentState): { _history: IterDocument[]; _future: IterDocument[] } {
  if (!s.document) return { _history: s._history, _future: [] };
  const history = [...s._history, s.document].slice(-MAX_HISTORY);
  return { _history: history, _future: [] };
}

export const useDocumentStore = create<DocumentState>((set, get) => ({
  document: null,
  diagnostics: [],
  warnings: [],
  currentFilePath: null,
  _savedSnapshot: null,
  _history: [],
  _future: [],

  setDocument: (document) => set((s) => ({
    document: normalize(document),
    ...pushHistory(s),
  })),
  setDiagnostics: (diagnostics, warnings = []) => set({ diagnostics: diagnostics ?? [], warnings: warnings ?? [] }),
  setCurrentFilePath: (currentFilePath) => set({ currentFilePath }),
  markSaved: () => set((s) => ({ _savedSnapshot: s.document ? JSON.stringify(s.document) : null })),
  isDirty: () => {
    const s = get();
    if (!s.document) return false;
    if (!s._savedSnapshot) return true;
    return JSON.stringify(s.document) !== s._savedSnapshot;
  },

  // Undo/redo
  undo: () => set((s) => {
    if (s._history.length === 0 || !s.document) return s;
    const history = [...s._history];
    const prev = history.pop()!;
    return { document: prev, _history: history, _future: [s.document, ...s._future].slice(0, MAX_HISTORY) };
  }),
  redo: () => set((s) => {
    if (s._future.length === 0 || !s.document) return s;
    const future = [...s._future];
    const next = future.shift()!;
    return { document: next, _history: [...s._history, s.document].slice(-MAX_HISTORY), _future: future };
  }),
  canUndo: () => get()._history.length > 0,
  canRedo: () => get()._future.length > 0,

  // Node updates
  updateAgent: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, agents: updateInArray(s.document.agents, name, updates) }, ...pushHistory(s) } : s)),
  updateJudge: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, judges: updateInArray(s.document.judges, name, updates) }, ...pushHistory(s) } : s)),
  updateRouter: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, routers: updateInArray(s.document.routers, name, updates) }, ...pushHistory(s) } : s)),
  updateJoin: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, joins: updateInArray(s.document.joins, name, updates) }, ...pushHistory(s) } : s)),
  updateHuman: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, humans: updateInArray(s.document.humans, name, updates) }, ...pushHistory(s) } : s)),
  updateTool: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, tools: updateInArray(s.document.tools, name, updates) }, ...pushHistory(s) } : s)),
  updateWorkflow: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: updateInArray(s.document.workflows, name, updates) }, ...pushHistory(s) } : s)),

  // Node add
  addAgent: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, agents: [...s.document.agents, decl] }, ...pushHistory(s) } : s)),
  addJudge: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, judges: [...s.document.judges, decl] }, ...pushHistory(s) } : s)),
  addRouter: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, routers: [...s.document.routers, decl] }, ...pushHistory(s) } : s)),
  addJoin: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, joins: [...s.document.joins, decl] }, ...pushHistory(s) } : s)),
  addHuman: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, humans: [...s.document.humans, decl] }, ...pushHistory(s) } : s)),
  addTool: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, tools: [...s.document.tools, decl] }, ...pushHistory(s) } : s)),

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
        ...pushHistory(s),
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
        ...pushHistory(s),
      };
    }),

  // Duplicate node — deep-clones with unique name, returns new name
  duplicateNode: (name) => {
    const s = get();
    if (!s.document) return null;
    const doc = s.document;
    // Collect all existing names
    const allNames = new Set<string>();
    for (const a of doc.agents) allNames.add(a.name);
    for (const j of doc.judges) allNames.add(j.name);
    for (const r of doc.routers) allNames.add(r.name);
    for (const j of doc.joins) allNames.add(j.name);
    for (const h of doc.humans) allNames.add(h.name);
    for (const t of doc.tools) allNames.add(t.name);
    // Generate unique name
    let i = 1;
    let newName = `${name}_copy`;
    while (allNames.has(newName)) { newName = `${name}_copy_${i}`; i++; }
    // Find and clone
    const cloneAgent = doc.agents.find((a) => a.name === name);
    if (cloneAgent) { set((st) => ({ document: { ...st.document!, agents: [...st.document!.agents, { ...cloneAgent, name: newName }] }, ...pushHistory(st) })); return newName; }
    const cloneJudge = doc.judges.find((j) => j.name === name);
    if (cloneJudge) { set((st) => ({ document: { ...st.document!, judges: [...st.document!.judges, { ...cloneJudge, name: newName }] }, ...pushHistory(st) })); return newName; }
    const cloneRouter = doc.routers.find((r) => r.name === name);
    if (cloneRouter) { set((st) => ({ document: { ...st.document!, routers: [...st.document!.routers, { ...cloneRouter, name: newName }] }, ...pushHistory(st) })); return newName; }
    const cloneJoinDecl = doc.joins.find((j) => j.name === name);
    if (cloneJoinDecl) { set((st) => ({ document: { ...st.document!, joins: [...st.document!.joins, { ...cloneJoinDecl, require: [...cloneJoinDecl.require], name: newName }] }, ...pushHistory(st) })); return newName; }
    const cloneHuman = doc.humans.find((h) => h.name === name);
    if (cloneHuman) { set((st) => ({ document: { ...st.document!, humans: [...st.document!.humans, { ...cloneHuman, name: newName }] }, ...pushHistory(st) })); return newName; }
    const cloneTool = doc.tools.find((t) => t.name === name);
    if (cloneTool) { set((st) => ({ document: { ...st.document!, tools: [...st.document!.tools, { ...cloneTool, name: newName }] }, ...pushHistory(st) })); return newName; }
    return null;
  },

  // Workflow management
  addWorkflow: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: [...s.document.workflows, decl] }, ...pushHistory(s) } : s)),
  removeWorkflow: (name) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: s.document.workflows.filter((w) => w.name !== name) }, ...pushHistory(s) } : s)),

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
        ...pushHistory(s),
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
        ...pushHistory(s),
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
        ...pushHistory(s),
      };
    }),

  // Schema mutations
  addSchema: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: [...s.document.schemas, decl] }, ...pushHistory(s) } : s)),
  removeSchema: (name) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: s.document.schemas.filter((d) => d.name !== name) }, ...pushHistory(s) } : s)),
  updateSchema: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, schemas: updateInArray(s.document.schemas, name, updates) }, ...pushHistory(s) } : s)),
  renameSchema: (oldName, newName) =>
    set((s) => {
      if (!s.document || oldName === newName) return s;
      const doc = s.document;
      const r = (v: string) => (v === oldName ? newName : v);
      return {
        document: {
          ...doc,
          schemas: doc.schemas.map((sc) => (sc.name === oldName ? { ...sc, name: newName } : sc)),
          agents: doc.agents.map((a) => ({ ...a, input: r(a.input), output: r(a.output) })),
          judges: doc.judges.map((j) => ({ ...j, input: r(j.input), output: r(j.output) })),
          joins: doc.joins.map((j) => ({ ...j, output: r(j.output) })),
          humans: doc.humans.map((h) => ({ ...h, input: r(h.input), output: r(h.output) })),
          tools: doc.tools.map((t) => ({ ...t, output: r(t.output) })),
        },
        ...pushHistory(s),
      };
    }),

  // Prompt mutations
  addPrompt: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: [...s.document.prompts, decl] }, ...pushHistory(s) } : s)),
  removePrompt: (name) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: s.document.prompts.filter((d) => d.name !== name) }, ...pushHistory(s) } : s)),
  updatePrompt: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: updateInArray(s.document.prompts, name, updates) }, ...pushHistory(s) } : s)),
  renamePrompt: (oldName, newName) =>
    set((s) => {
      if (!s.document || oldName === newName) return s;
      const doc = s.document;
      const r = (v: string) => (v === oldName ? newName : v);
      const ro = (v?: string) => (v === oldName ? newName : v);
      return {
        document: {
          ...doc,
          prompts: doc.prompts.map((p) => (p.name === oldName ? { ...p, name: newName } : p)),
          agents: doc.agents.map((a) => ({ ...a, system: r(a.system), user: r(a.user) })),
          judges: doc.judges.map((j) => ({ ...j, system: r(j.system), user: r(j.user) })),
          humans: doc.humans.map((h) => ({ ...h, instructions: r(h.instructions), system: ro(h.system) })),
        },
        ...pushHistory(s),
      };
    }),

  // Vars mutations
  setVars: (vars) =>
    set((s) => (s.document ? { document: { ...s.document, vars }, ...pushHistory(s) } : s)),
  setWorkflowVars: (workflowName, vars) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) => (w.name === workflowName ? { ...w, vars } : w)),
        },
        ...pushHistory(s),
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
        ...pushHistory(s),
      };
    }),

  // Comment mutations
  addComment: (comment) =>
    set((s) => (s.document ? { document: { ...s.document, comments: [...s.document.comments, comment] }, ...pushHistory(s) } : s)),
  removeComment: (index) =>
    set((s) => (s.document ? { document: { ...s.document, comments: s.document.comments.filter((_, i) => i !== index) }, ...pushHistory(s) } : s)),
  updateComment: (index, text) =>
    set((s) => (s.document ? { document: { ...s.document, comments: s.document.comments.map((c, i) => i === index ? { ...c, text } : c) }, ...pushHistory(s) } : s)),
}));
