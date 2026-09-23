import { createContext, useContext, type ReactNode, createElement } from "react";
import { create, useStore } from "zustand";
import type {
  IterDocument,
  AgentDecl,
  JudgeDecl,
  RouterDecl,
  HumanDecl,
  ToolNodeDecl,
  ComputeDecl,
  SubbotDecl,
  WorkflowDecl,
  SchemaDecl,
  PromptDecl,
  CursorDecl,
  VarsBlock,
  BudgetBlock,
  CompactionBlock,
  MCPServerDecl,
  Edge,
  Comment,
  UnitInfo,
} from "@/api/types";
import type { DiagnosticIssue } from "@/api/client";
import { createEmptyDocument, getAllNodeNames, getAllSchemaNames, getAllPromptNames, findNodeDecl } from "@/lib/defaults";
import type { GroupAnnotation } from "@/lib/groups";
import {
  documentComments,
  documentGroups,
  groupNameFromComment,
  groupToCommentText,
  mapDocumentComments,
  parseGroups,
} from "@/lib/groups";

// Normalize a document from JSON (omitempty may leave arrays as undefined).
function normalize(doc: IterDocument): IterDocument {
  return {
    ...doc,
    mcp_servers: doc.mcp_servers ?? [],
    prompts: doc.prompts ?? [],
    schemas: doc.schemas ?? [],
    cursors: doc.cursors ?? [],
    agents: doc.agents ?? [],
    judges: doc.judges ?? [],
    routers: doc.routers ?? [],
    humans: doc.humans ?? [],
    tools: doc.tools ?? [],
    computes: doc.computes ?? [],
    subbots: doc.subbots ?? [],
    workflows: (doc.workflows ?? []).map((w) => ({
      ...w,
      edges: w.edges ?? [],
    })),
    comments: doc.comments ?? [],
  };
}

const MAX_HISTORY = 50;

/** The Source view's open text edit, as the rest of the studio sees it.
 *  `base` is the text the render produced; `text` is what the author has
 *  since typed. They part exactly when there is work a discard would take. */
export interface SourceBuffer {
  /** The tab's file the buffer belongs to, and — for a bot in several files
   *  — which file of its unit. They are what lets the view recognise its own
   *  buffer when it mounts again. */
  path: string | null;
  rel: string | null;
  text: string;
  base: string;
  /** The document the buffer was rendered FROM. Carried so that a view
   *  re-adopting it restores the provenance too: an Apply is refused when
   *  the document moved under the text, and a re-adopted buffer that
   *  claimed the CURRENT document would lose exactly that refusal. */
  doc: IterDocument | null;
}

interface DocumentState {
  document: IterDocument | null;
  diagnostics: string[];
  warnings: string[];
  /** Structured diagnostics from the Go validator (Phase 7). When present,
   *  these supersede the heuristic attribution applied to `diagnostics` /
   *  `warnings`. Empty for parser-only responses. */
  issues: DiagnosticIssue[];
  currentFilePath: string | null;
  /** True when `document` is what the parser SALVAGED — the file minus the
   *  region it could not read — rather than the file. The path stays: this
   *  tab is still about that file, and everything that asks WHICH file goes
   *  on working. What it forbids is WRITING: a save would put the salvage
   *  over what the author wrote. Cleared by a parse of the buffer that comes
   *  back whole. */
  salvaged: boolean;
  /** True once the path was SET to null — File → New, Import, Start blank:
   *  the document follows no file. A fresh store's null path is not that:
   *  it means "not resolved yet", and a tab keeps the file param its load is
   *  for through that window. The two nulls decide the tab's params
   *  (TabBindingSync): a detached document drops them, an unresolved one
   *  leaves them. Cleared by binding a path. */
  detached: boolean;
  // Cached so cloud-mode launch/resume can pass it inline. Updated on
  // openFile / saveFile / parseSource; null otherwise.
  currentSource: string | null;
  // The unit the document was opened from, for a bot in several files
  // (its files and the revision a save must present); null otherwise.
  // Dropped whenever the current file changes: a unit belongs to a file.
  unit: UnitInfo | null;
  // The Source view's open, un-applied text edit — the buffer itself, not a
  // flag about it. It used to be component-local `useState`, so nothing
  // outside could see it: `_generation` does not move while an author types
  // there, `isDirty()` reported false, and every discard path — closing the
  // tab, opening another file, the watcher's reload, the assistant's
  // reload-after-write — took the text with no prompt and no undo (#1662).
  // One value rather than a flag beside a buffer: "is an edit open" and "is
  // it dirty" are read by different surfaces, and two fields would drift.
  sourceBuffer: SourceBuffer | null;
  _generation: number;
  _savedGeneration: number;

  // Undo/redo
  _history: IterDocument[];
  _future: IterDocument[];

  // Document lifecycle
  setDocument: (doc: IterDocument) => void;
  setDiagnostics: (d: string[], w?: string[], issues?: DiagnosticIssue[]) => void;
  setCurrentFilePath: (path: string | null) => void;
  setSalvaged: (salvaged: boolean) => void;
  setCurrentSource: (source: string | null) => void;
  setUnit: (unit: UnitInfo | null) => void;
  setSourceBuffer: (buffer: SourceBuffer | null) => void;
  markSaved: () => void;
  isDirty: () => boolean;
  /** The Source view's buffer holds text its render did not produce. */
  isSourceDirty: () => boolean;
  /** Unsaved work of ANY kind in this tab — the document, or the Source
   *  view's un-applied text. What a discard path consults. */
  hasUnsavedWork: () => boolean;


  // Undo/redo
  undo: () => void;
  redo: () => void;
  canUndo: () => boolean;
  canRedo: () => boolean;

  // Node updates
  updateAgent: (name: string, updates: Partial<AgentDecl>) => void;
  updateJudge: (name: string, updates: Partial<JudgeDecl>) => void;
  updateRouter: (name: string, updates: Partial<RouterDecl>) => void;
  updateHuman: (name: string, updates: Partial<HumanDecl>) => void;
  updateTool: (name: string, updates: Partial<ToolNodeDecl>) => void;
  updateCompute: (name: string, updates: Partial<ComputeDecl>) => void;
  updateSubbot: (name: string, updates: Partial<SubbotDecl>) => void;
  updateWorkflow: (name: string, updates: Partial<WorkflowDecl>) => void;

  // Node add/remove
  addAgent: (decl: AgentDecl) => void;
  addJudge: (decl: JudgeDecl) => void;
  addRouter: (decl: RouterDecl) => void;
  addHuman: (decl: HumanDecl) => void;
  addTool: (decl: ToolNodeDecl) => void;
  addCompute: (decl: ComputeDecl) => void;
  addSubbot: (decl: SubbotDecl) => void;
  removeNode: (name: string) => void;
  renameNode: (oldName: string, newName: string) => void;
  duplicateNode: (name: string) => string | null;

  // Workflow management
  addWorkflow: (decl: WorkflowDecl) => void;
  removeWorkflow: (name: string) => void;

  // Edge mutations
  addEdge: (workflowName: string, edge: Edge) => void;
  removeEdge: (workflowName: string, edgeIndex: number, fromHint?: string, toHint?: string) => void;
  updateEdge: (workflowName: string, edgeIndex: number, updates: Partial<Edge>) => void;

  // Schema mutations
  addSchema: (decl: SchemaDecl) => void;
  removeSchema: (name: string) => void;
  updateSchema: (name: string, updates: Partial<SchemaDecl>) => void;
  renameSchema: (oldName: string, newName: string) => void;

  // Cursor mutations (top-level `cursor <name>:` declarations)
  addCursorDecl: (decl: CursorDecl) => void;
  removeCursorDecl: (name: string) => void;
  updateCursorDecl: (name: string, updates: Partial<CursorDecl>) => void;

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

  // Compaction mutations
  updateWorkflowCompaction: (workflowName: string, compaction: CompactionBlock | undefined) => void;

  // Top-level MCP server declarations
  addMCPServer: (decl: MCPServerDecl) => void;
  removeMCPServer: (name: string) => void;
  updateMCPServer: (name: string, updates: Partial<MCPServerDecl>) => void;

  // Comment mutations
  addComment: (comment: Comment) => void;
  removeComment: (index: number) => void;
  updateComment: (index: number, text: string) => void;

  // Batch mutation (single undo step for multi-declaration changes like library items)
  applyBatch: (mutator: (doc: IterDocument) => IterDocument) => void;

  // Group operations (manipulate @group comments)
  addGroup: (group: GroupAnnotation) => void;
  removeGroup: (groupName: string) => void;
  updateGroup: (groupName: string, updates: Partial<GroupAnnotation>) => void;
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
function pushHistory(s: DocumentState): { _history: IterDocument[]; _future: IterDocument[]; _generation: number } {
  if (!s.document) return { _history: s._history, _future: [], _generation: s._generation + 1 };
  const history = [...s._history, s.document].slice(-MAX_HISTORY);
  return { _history: history, _future: [], _generation: s._generation + 1 };
}

/** Drop a node from every @group comment of a document, wherever the comment
 *  lives. A group that falls below 2 members dissolves. */
function dropNodeFromGroups(doc: IterDocument, nodeName: string): IterDocument {
  return mapDocumentComments(doc, (c) => {
    if (!groupNameFromComment(c)) return c;
    const g = parseGroups([c])[0];
    if (!g) return c;
    const remaining = g.nodeIds.filter((id) => id !== nodeName);
    if (remaining.length < 2) return null; // dissolve group
    return { ...c, text: groupToCommentText({ ...g, nodeIds: remaining }) };
  });
}

/** Rewrite the document's groups for a node that is being removed.
 *
 *  A group's comment is carried by whatever the author wrote it next to —
 *  a declaration, an edge, the file's head. Removing a node takes its
 *  declaration and every edge touching it, so a group whose comment happened
 *  to sit there would go with them even though the members that are LEFT are
 *  still on the canvas. The comparison is made against what the group would
 *  have become had its carrier survived, and any group that is missing after
 *  the removal is re-declared on the document's own comment list — where the
 *  studio writes the groups it creates, and where the save puts it above the
 *  `dsl:` header. Keyed on group NAMES, so it covers every carrier kind
 *  without naming any of them. */
function removeNodeFromGroups(before: IterDocument, after: IterDocument, nodeName: string): IterDocument {
  const doc = dropNodeFromGroups(after, nodeName);
  const kept = dropNodeFromGroups(before, nodeName);
  const survivors = documentComments(kept).filter((c) => groupNameFromComment(c));
  if (survivors.length === 0) return doc;
  const present = new Set(documentGroups(doc).map((g) => g.name));
  // The comment is re-declared with its `file` — a comment with none is
  // written to the main, so dropping it would move the author's line out of
  // the fragment they wrote it in and into main.bot, on a save they asked
  // for one node of.
  const orphaned = survivors.filter((c) => {
    const name = groupNameFromComment(c);
    return !!name && !present.has(name);
  });
  if (orphaned.length === 0) return doc;
  return {
    ...doc,
    comments: [...doc.comments, ...orphaned.map((c) => ({ ...c, anchor: undefined, place: undefined }))],
  };
}

/** Rename a node in every @group comment of the document, wherever it lives. */
function renameNodeInGroups(doc: IterDocument, oldName: string, newName: string): IterDocument {
  return mapDocumentComments(doc, (c) => {
    if (!groupNameFromComment(c)) return c;
    const g = parseGroups([c])[0];
    if (!g) return c;
    if (!g.nodeIds.includes(oldName)) return c;
    return { ...c, text: groupToCommentText({ ...g, nodeIds: g.nodeIds.map((id) => (id === oldName ? newName : id)) }) };
  });
}

// createDocumentStore builds a fresh Zustand store with the existing
// reducer/action surface. Each editor tab owns its own store so two
// .bot files can be edited side-by-side with independent dirty
// state, undo history, and diagnostics. The module-level
// `documentStore` façade preserves the legacy singleton entry point
// for App-level callers and imperative side-effects.
export function createDocumentStore() {
  return create<DocumentState>((set, get) => ({
  document: normalize(createEmptyDocument()),
  diagnostics: [],
  warnings: [],
  issues: [],
  currentFilePath: null,
  salvaged: false,
  detached: false,
  currentSource: null,
  unit: null,
  sourceBuffer: null,
  _generation: 0,
  _savedGeneration: 0,
  _history: [],
  _future: [],

  setDocument: (document) => set((s) => ({
    document: normalize(document),
    ...pushHistory(s),
  })),
  setDiagnostics: (diagnostics, warnings = [], issues = []) =>
    set({
      diagnostics: diagnostics ?? [],
      warnings: warnings ?? [],
      issues: issues ?? [],
    }),
  // A new file is a new program: whatever the last one salvaged says nothing
  // about this one, so the flag is dropped with the unit. A null path here is
  // a detachment — told apart from a fresh store's null by `detached`.
  setCurrentFilePath: (currentFilePath) =>
    set({ currentFilePath, unit: null, salvaged: false, sourceBuffer: null, detached: currentFilePath === null }),
  setSalvaged: (salvaged) => set({ salvaged }),
  setCurrentSource: (currentSource) => set((s) => (s.currentSource === currentSource ? s : { currentSource })),
  setUnit: (unit) => set({ unit }),
  setSourceBuffer: (sourceBuffer) => set({ sourceBuffer }),
  markSaved: () => set((s) => ({ _savedGeneration: s._generation })),
  isDirty: () => {
    const s = get();
    if (!s.document) return false;
    return s._generation !== s._savedGeneration;
  },
  isSourceDirty: () => {
    const b = get().sourceBuffer;
    return !!b && b.text !== b.base;
  },
  // What every path that would DESTROY the author's work has to ask. Not
  // `isDirty()` on its own: the document and the Source view's buffer hold
  // unsaved work independently, and a reload, a tab close or a File → New
  // takes both. The Source view's own prompts deliberately keep asking
  // `isDirty()` instead — they ask whether the DOCUMENT holds something
  // their text does not carry, and a buffer that saw itself would prompt on
  // every Apply.
  hasUnsavedWork: () => {
    const s = get();
    return s.isDirty() || s.isSourceDirty();
  },

  // Undo/redo
  undo: () => set((s) => {
    if (s._history.length === 0 || !s.document) return s;
    const history = [...s._history];
    const prev = history.pop();
    if (!prev) return s;
    return { document: prev, _history: history, _future: [s.document, ...s._future].slice(0, MAX_HISTORY), _generation: s._generation + 1 };
  }),
  redo: () => set((s) => {
    if (s._future.length === 0 || !s.document) return s;
    const future = [...s._future];
    const next = future.shift();
    if (!next) return s;
    return { document: next, _history: [...s._history, s.document].slice(-MAX_HISTORY), _future: future, _generation: s._generation + 1 };
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
  updateHuman: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, humans: updateInArray(s.document.humans, name, updates) }, ...pushHistory(s) } : s)),
  updateTool: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, tools: updateInArray(s.document.tools, name, updates) }, ...pushHistory(s) } : s)),
  updateCompute: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, computes: updateInArray(s.document.computes, name, updates) }, ...pushHistory(s) } : s)),
  updateSubbot: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, subbots: updateInArray(s.document.subbots ?? [], name, updates) }, ...pushHistory(s) } : s)),
  updateWorkflow: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: updateInArray(s.document.workflows, name, updates) }, ...pushHistory(s) } : s)),

  // Node add
  addAgent: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, agents: [...s.document.agents, decl] }, ...pushHistory(s) } : s)),
  addJudge: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, judges: [...s.document.judges, decl] }, ...pushHistory(s) } : s)),
  addRouter: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, routers: [...s.document.routers, decl] }, ...pushHistory(s) } : s)),
  addHuman: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, humans: [...s.document.humans, decl] }, ...pushHistory(s) } : s)),
  addTool: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, tools: [...s.document.tools, decl] }, ...pushHistory(s) } : s)),
  addCompute: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, computes: [...s.document.computes, decl] }, ...pushHistory(s) } : s)),
  addSubbot: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, subbots: [...(s.document.subbots ?? []), decl] }, ...pushHistory(s) } : s)),

  // Node remove — removes declaration + cleans up all edges referencing it
  removeNode: (name) =>
    set((s) => {
      if (!s.document) return s;
      const doc = s.document;
      return {
        // The group rewrite runs LAST, over the document the removal
        // leaves: a @group annotation carried by a declaration that is
        // still there has to lose the node too, and one carried by the
        // declaration just removed is gone with it.
        document: removeNodeFromGroups(
          doc,
          {
            ...doc,
            agents: doc.agents.filter((a) => a.name !== name),
            judges: doc.judges.filter((j) => j.name !== name),
            routers: doc.routers.filter((r) => r.name !== name),
            humans: doc.humans.filter((h) => h.name !== name),
            tools: doc.tools.filter((t) => t.name !== name),
            computes: doc.computes.filter((c) => c.name !== name),
            subbots: (doc.subbots ?? []).filter((sb) => sb.name !== name),
            workflows: removeNodeEdges(doc, name),
          },
          name,
        ),
        ...pushHistory(s),
      };
    }),

  // Node rename — updates all references
  renameNode: (oldName, newName) =>
    set((s) => {
      if (!s.document || oldName === newName || !newName.trim()) return s;
      const doc = s.document;
      // Guard: reject duplicate names
      const existing = getAllNodeNames(doc);
      existing.delete(oldName);
      if (existing.has(newName)) return s;
      const renameIn = <T extends { name: string }>(arr: T[]) =>
        arr.map((item) => (item.name === oldName ? { ...item, name: newName } : item));
      return {
        document: renameNodeInGroups(
          {
            ...doc,
            agents: renameIn(doc.agents),
            judges: renameIn(doc.judges),
            routers: renameIn(doc.routers),
            humans: renameIn(doc.humans),
            tools: renameIn(doc.tools),
            computes: renameIn(doc.computes),
            subbots: renameIn(doc.subbots ?? []),
            workflows: updateWorkflowsEdges(doc, oldName, newName),
          },
          oldName,
          newName,
        ),
        ...pushHistory(s),
      };
    }),

  // Duplicate node — deep-clones with unique name, returns new name
  duplicateNode: (name) => {
    const s = get();
    if (!s.document) return null;
    const doc = s.document;
    const found = findNodeDecl(doc, name);
    if (!found) return null;

    const allNames = getAllNodeNames(doc);
    let i = 1;
    let newName = `${name}_copy`;
    while (allNames.has(newName)) { newName = `${name}_copy_${i}`; i++; }

    // Deep-clone with new name, copying nested arrays to avoid shared references
    const clone = { ...found.decl, name: newName };
    if ("tools" in clone && Array.isArray(clone.tools)) clone.tools = [...clone.tools];
    // The copy keeps the author's comments but NOT a @group annotation: a
    // group names its members by id, so a verbatim copy declares a second
    // group of the same name — two canvas nodes sharing one React Flow id,
    // and the line written twice into the .bot on save.
    if ("comments" in clone && Array.isArray(clone.comments)) {
      clone.comments = (clone.comments as Comment[]).filter((c) => !groupNameFromComment(c));
    }
    const kindToArray: Record<string, keyof IterDocument> = {
      agent: "agents", judge: "judges", router: "routers",
      human: "humans", tool: "tools", compute: "computes",
      subbot: "subbots",
    };
    const arrayKey = kindToArray[found.kind];
    if (!arrayKey) return null;

    set((st) => {
      if (!st.document) return st;
      return {
        document: {
          ...st.document,
          [arrayKey]: [...(st.document[arrayKey] as unknown[]), clone],
        },
        ...pushHistory(st),
      };
    });
    return newName;
  },

  // Workflow management
  addWorkflow: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, workflows: [...s.document.workflows, decl] }, ...pushHistory(s) } : s)),
  removeWorkflow: (name) =>
    set((s) => {
      if (!s.document) return s;
      const doc = s.document;
      const remainingWorkflows = doc.workflows.filter((w) => w.name !== name);
      // Collect node names still referenced by remaining workflows
      const referencedNodes = new Set<string>();
      for (const wf of remainingWorkflows) {
        if (wf.entry) referencedNodes.add(wf.entry);
        for (const e of wf.edges) {
          referencedNodes.add(e.from);
          referencedNodes.add(e.to);
        }
      }
      // Remove orphan nodes (not referenced by any remaining workflow)
      const isReferenced = (nodeName: string) => remainingWorkflows.length === 0 || referencedNodes.has(nodeName);
      return {
        document: {
          ...doc,
          workflows: remainingWorkflows,
          agents: doc.agents.filter((a) => isReferenced(a.name)),
          judges: doc.judges.filter((j) => isReferenced(j.name)),
          routers: doc.routers.filter((r) => isReferenced(r.name)),
          humans: doc.humans.filter((h) => isReferenced(h.name)),
          tools: doc.tools.filter((t) => isReferenced(t.name)),
          computes: doc.computes.filter((c) => isReferenced(c.name)),
          subbots: (doc.subbots ?? []).filter((sb) => isReferenced(sb.name)),
        },
        ...pushHistory(s),
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
        ...pushHistory(s),
      };
    }),

  removeEdge: (workflowName, edgeIndex, fromHint?, toHint?) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) => {
            if (w.name !== workflowName) return w;
            // Prefer index match; if stale, fall back to from+to identity match
            if (w.edges[edgeIndex] &&
              (!fromHint || w.edges[edgeIndex].from === fromHint) &&
              (!toHint || w.edges[edgeIndex].to === toHint)) {
              return { ...w, edges: w.edges.filter((_, i) => i !== edgeIndex) };
            }
            // Fallback: remove first edge matching from+to
            if (fromHint && toHint) {
              let found = false;
              return { ...w, edges: w.edges.filter((e) => {
                if (!found && e.from === fromHint && e.to === toHint) { found = true; return false; }
                return true;
              })};
            }
            return { ...w, edges: w.edges.filter((_, i) => i !== edgeIndex) };
          }),
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
      if (!s.document || oldName === newName || !newName.trim()) return s;
      const doc = s.document;
      // Guard: reject duplicate names
      const existingSchemas = getAllSchemaNames(doc);
      existingSchemas.delete(oldName);
      if (existingSchemas.has(newName)) return s;
      const r = (v: string) => (v === oldName ? newName : v);
      const ro = (v?: string) => (v === oldName ? newName : v);
      return {
        document: {
          ...doc,
          schemas: doc.schemas.map((sc) => (sc.name === oldName ? { ...sc, name: newName } : sc)),
          agents: doc.agents.map((a) => ({ ...a, input: r(a.input), output: r(a.output) })),
          judges: doc.judges.map((j) => ({ ...j, input: r(j.input), output: r(j.output) })),
          humans: doc.humans.map((h) => ({ ...h, input: r(h.input), output: r(h.output) })),
          tools: doc.tools.map((t) => ({ ...t, input: ro(t.input), output: r(t.output) })),
          computes: doc.computes.map((c) => ({ ...c, input: ro(c.input), output: r(c.output) })),
        },
        ...pushHistory(s),
      };
    }),

  // Cursor mutations
  addCursorDecl: (decl) =>
    set((s) =>
      s.document
        ? {
            document: { ...s.document, cursors: [...(s.document.cursors ?? []), decl] },
            ...pushHistory(s),
          }
        : s,
    ),
  removeCursorDecl: (name) =>
    set((s) =>
      s.document
        ? {
            document: { ...s.document, cursors: (s.document.cursors ?? []).filter((c) => c.name !== name) },
            ...pushHistory(s),
          }
        : s,
    ),
  updateCursorDecl: (name, updates) =>
    set((s) =>
      s.document
        ? {
            document: {
              ...s.document,
              cursors: (s.document.cursors ?? []).map((c) => (c.name === name ? { ...c, ...updates } : c)),
            },
            ...pushHistory(s),
          }
        : s,
    ),

  // Prompt mutations
  addPrompt: (decl) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: [...s.document.prompts, decl] }, ...pushHistory(s) } : s)),
  removePrompt: (name) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: s.document.prompts.filter((d) => d.name !== name) }, ...pushHistory(s) } : s)),
  updatePrompt: (name, updates) =>
    set((s) => (s.document ? { document: { ...s.document, prompts: updateInArray(s.document.prompts, name, updates) }, ...pushHistory(s) } : s)),
  renamePrompt: (oldName, newName) =>
    set((s) => {
      if (!s.document || oldName === newName || !newName.trim()) return s;
      const doc = s.document;
      // Guard: reject duplicate names
      const existingPrompts = getAllPromptNames(doc);
      existingPrompts.delete(oldName);
      if (existingPrompts.has(newName)) return s;
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

  // Compaction mutations (workflow-level)
  updateWorkflowCompaction: (workflowName, compaction) =>
    set((s) => {
      if (!s.document) return s;
      return {
        document: {
          ...s.document,
          workflows: s.document.workflows.map((w) => (w.name === workflowName ? { ...w, compaction } : w)),
        },
        ...pushHistory(s),
      };
    }),

  // Top-level MCP server declarations. The `mcp_servers` array is
  // shared across all workflows in the file; per-node activation
  // happens via the node's `mcp` field which references these by name.
  addMCPServer: (decl) =>
    set((s) =>
      s.document
        ? { document: { ...s.document, mcp_servers: [...(s.document.mcp_servers ?? []), decl] }, ...pushHistory(s) }
        : s,
    ),
  removeMCPServer: (name) =>
    set((s) =>
      s.document
        ? {
            document: {
              ...s.document,
              mcp_servers: (s.document.mcp_servers ?? []).filter((d) => d.name !== name),
            },
            ...pushHistory(s),
          }
        : s,
    ),
  updateMCPServer: (name, updates) =>
    set((s) =>
      s.document
        ? {
            document: {
              ...s.document,
              mcp_servers: updateInArray(s.document.mcp_servers ?? [], name, updates),
            },
            ...pushHistory(s),
          }
        : s,
    ),

  // Comment mutations
  addComment: (comment) =>
    set((s) => (s.document ? { document: { ...s.document, comments: [...s.document.comments, comment] }, ...pushHistory(s) } : s)),
  removeComment: (index) =>
    set((s) => (s.document ? { document: { ...s.document, comments: s.document.comments.filter((_, i) => i !== index) }, ...pushHistory(s) } : s)),
  updateComment: (index, text) =>
    set((s) => (s.document ? { document: { ...s.document, comments: s.document.comments.map((c, i) => i === index ? { ...c, text } : c) }, ...pushHistory(s) } : s)),

  // Batch mutation — applies a document transform as a single undo step
  applyBatch: (mutator) =>
    set((s) => {
      if (!s.document) return s;
      return { document: normalize(mutator(s.document)), ...pushHistory(s) };
    }),

  // Group operations — groups are stored as @group comments. A group the
  // studio CREATES goes on the document's own list, which the save writes
  // above the `dsl:` header; one the AUTHOR wrote is wherever they put it,
  // so every operation that reaches an existing group goes through
  // `documentGroups`/`mapDocumentComments` rather than the head list.
  addGroup: (group) =>
    set((s) => {
      if (!s.document) return s;
      // Check for duplicate group name
      const existing = documentGroups(s.document);
      if (existing.some((g) => g.name === group.name)) return s;
      const comment: Comment = { text: groupToCommentText(group) };
      return { document: { ...s.document, comments: [...s.document.comments, comment] }, ...pushHistory(s) };
    }),

  removeGroup: (groupName) =>
    set((s) => {
      if (!s.document) return s;
      const document = mapDocumentComments(s.document, (c) =>
        groupNameFromComment(c) === groupName ? null : c,
      );
      return { document, ...pushHistory(s) };
    }),

  updateGroup: (groupName, updates) =>
    set((s) => {
      if (!s.document) return s;
      const document = mapDocumentComments(s.document, (c) => {
        if (groupNameFromComment(c) !== groupName) return c;
        const first = parseGroups([c])[0];
        if (!first) return c;
        const updated = { ...first, ...updates };
        return { ...c, text: groupToCommentText(updated) };
      });
      return { document, ...pushHistory(s) };
    }),
  }));
}

export type DocumentStore = ReturnType<typeof createDocumentStore>;

// Default store used by call sites that don't have a DocumentStoreProvider
// in their React tree (App-level hooks, useFileWatcher in single-tab mode,
// etc.). Per-editor-tab stores are created via EditorTabHost and override
// this default for components rendered inside that subtree.
const defaultDocumentStore = createDocumentStore();

const DocumentStoreContext = createContext<DocumentStore | null>(null);

interface DocumentStoreProviderProps {
  store: DocumentStore;
  children: ReactNode;
}

export function DocumentStoreProvider({ store, children }: DocumentStoreProviderProps) {
  return createElement(DocumentStoreContext.Provider, { value: store }, children);
}

export function useDocumentStoreInstance(): DocumentStore {
  return useContext(DocumentStoreContext) ?? defaultDocumentStore;
}

// useDocumentStore preserves the (s) => x selector API from the singleton
// era so call sites don't change. Inside a Provider it reads from that
// provider's store; outside, it falls back to the module-level default.
export function useDocumentStore<T>(selector: (state: DocumentState) => T): T {
  return useStore(useDocumentStoreInstance(), selector);
}

// Imperative façade for non-React callers. Pre-tab callers used
// `useDocumentStore.getState()` — that pattern is preserved via this
// helper wrapping the default store. Per-tab stores are reached via
// useDocumentStoreInstance() from within an EditorTabHost subtree.
export const documentStore = {
  getState: () => defaultDocumentStore.getState(),
  setState: defaultDocumentStore.setState,
  subscribe: defaultDocumentStore.subscribe,
};

// Registry of stores keyed by tab id. EditorTabHost looks up its
// store via getOrCreateDocumentStore(tabId) on mount; closeTab in
// useTabsStore is responsible for calling disposeDocumentStore so
// the store is reclaimed once the tab is gone.
const REGISTRY = new Map<string, DocumentStore>();

export function getOrCreateDocumentStore(tabId: string): DocumentStore {
  let store = REGISTRY.get(tabId);
  if (!store) {
    store = createDocumentStore();
    REGISTRY.set(tabId, store);
  }
  return store;
}

// Read-only registry lookup for cross-cutting UI capabilities (the assistant
// editor bridge). Unlike getOrCreateDocumentStore it never manufactures an
// empty document for a stale/restored tab id: absence means there is no live
// editor session to bind an action to.
export function getDocumentStore(tabId: string): DocumentStore | undefined {
  return REGISTRY.get(tabId);
}

export function disposeDocumentStore(tabId: string): void {
  REGISTRY.delete(tabId);
}
