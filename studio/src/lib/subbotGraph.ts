import type { Node, Edge as FlowEdge } from "@xyflow/react";
import type { IterDocument, SubbotDecl } from "@/api/types";
import { documentToGraph, getTopologyKey } from "./documentToGraph";
import type { NodeData } from "./documentToGraph";
import { NODE_COLORS } from "./constants";

// Pure expansion of subbot nodes into inline child-workflow frames on the
// EDITOR canvas. A `subbot` declaration runs another .bot file as a child
// run; this module replaces the compact subbot node with a container frame
// (type "subbotFrame") holding the child bot's whole graph, and rewires the
// parent's edges so the flow reads end-to-end:
//   dispatch -> [frame: produce -> review -> wrap -> done] -> collect
//
// Expansion is RECURSIVE (a subbot inside a subbot nests a frame inside the
// frame), bounded by MAX_SUBBOT_EXPANSION_DEPTH and a cycle guard on the
// resolved source paths (a.bot -> b.bot -> a.bot stops at the revisit,
// which stays a compact node with a cycle notice).

/** Separator between a subbot node id and a child node id. DSL identifiers
 *  cannot contain ":" so `id.includes("::")` is a reliable external marker.
 *  Nested children chain the separator: `stage::step::work`. */
export const SUBBOT_CHILD_SEP = "::";

/** Levels of frames displayed. Deeper subbots stay compact (the runtime
 *  allows depth 8, but 3 nested frames is already at the edge of legibility
 *  — beyond that the operator opens the child file / child run). */
export const MAX_SUBBOT_EXPANSION_DEPTH = 3;

export function makeSubbotChildId(subbotId: string, childId: string): string {
  return `${subbotId}${SUBBOT_CHILD_SEP}${childId}`;
}

/** True for nodes that belong to an expanded subbot's child graph. These are
 *  display-only: not editable, not connectable, not drill-in targets. */
export function isSubbotChildId(id: string): boolean {
  return id.includes(SUBBOT_CHILD_SEP);
}

/** Splits a child node id into { subbotId, childId } on the FIRST separator
 *  (subbotId = root-level frame). Null for plain ids. */
export function parseSubbotChildId(id: string): { subbotId: string; childId: string } | null {
  const idx = id.indexOf(SUBBOT_CHILD_SEP);
  if (idx <= 0) return null;
  return { subbotId: id.slice(0, idx), childId: id.slice(idx + SUBBOT_CHILD_SEP.length) };
}

/** Frame-local display name: the last segment of a (possibly nested)
 *  expanded id — `stage::step::work` renders as `work` inside its frame. */
export function subbotLocalName(id: string): string {
  const idx = id.lastIndexOf(SUBBOT_CHILD_SEP);
  return idx === -1 ? id : id.slice(idx + SUBBOT_CHILD_SEP.length);
}

/** One loaded (or failed) child document, keyed by its RESOLVED
 *  workspace-relative path — the same file expanded from two subbots (or
 *  two nesting levels) loads once. Loading = no entry in the map. */
export interface SubbotDocEntry {
  doc?: IterDocument;
  error?: string;
}

export interface SubbotFrameData extends Record<string, unknown> {
  label: string;
  kind: "subbot";
  color: string;
  decl: SubbotDecl;
  source: string;
  // Resolved workspace-relative path of the child file (for the open button).
  sourcePath: string;
  isolated: boolean;
  childWorkflowName: string;
}

// Initial frame size before ELK sizes the compound node (mirrors the
// expanded-group placeholder in applyGroups).
const FRAME_INITIAL_W = 420;
const FRAME_INITIAL_H = 320;

/** POSIX-join a subbot `source` against the directory of the parent file.
 *  Both inputs are workspace-relative; the result is normalized (no ".",
 *  ".." collapsed — never escaping above the workspace root). */
export function resolveSubbotSource(parentFilePath: string | null, source: string): string {
  const dir = parentFilePath ? parentFilePath.split("/").slice(0, -1).join("/") : "";
  const joined = dir ? `${dir}/${source}` : source;
  const parts: string[] = [];
  for (const seg of joined.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (parts.length > 0) parts.pop();
      continue;
    }
    parts.push(seg);
  }
  return parts.join("/");
}

/**
 * Resolve the file opened by the Inspector's "Open child bot" action.
 * The document store path is authoritative; the active tab binding covers
 * the short hydration window before EditorTabHost copies that path into the
 * store. With neither parent, returning null is safer than treating a
 * parent-relative source as workspace-relative.
 */
export function resolveChildBotOpenPath(
  documentFilePath: string | null,
  activeEditorFile: string | null,
  source: string,
): string | null {
  const parentFilePath = documentFilePath ?? activeEditorFile;
  return parentFilePath
    ? resolveSubbotSource(parentFilePath, source)
    : null;
}

/** Topology fingerprint of the expansion state: which subbots have a loaded
 *  (or failed) child document, and each loaded child's own topology —
 *  RECURSIVELY, so a nested child doc arriving re-fires the editor's ELK
 *  relayout. Cycle-guarded like the expansion itself. */
export function getSubbotExpansionKey(
  doc: IterDocument | null,
  docPath: string | null,
  docsByPath: Map<string, SubbotDocEntry>,
): string {
  if (!doc) return "";
  return expansionKeyRec(doc, docPath, docsByPath, new Set(docPath ? [docPath] : []), 0);
}

function expansionKeyRec(
  doc: IterDocument,
  docPath: string | null,
  docsByPath: Map<string, SubbotDocEntry>,
  ancestry: Set<string>,
  depth: number,
): string {
  const subbots = doc.subbots ?? [];
  if (subbots.length === 0) return "";
  const parts: string[] = [];
  for (const sb of subbots) {
    if (!sb.source) {
      parts.push(`${sb.name}:nosource`);
      continue;
    }
    const resolved = resolveSubbotSource(docPath, sb.source);
    const child = docsByPath.get(resolved);
    if (depth >= MAX_SUBBOT_EXPANSION_DEPTH || ancestry.has(resolved)) {
      parts.push(`${sb.name}:capped`);
    } else if (!child) {
      parts.push(`${sb.name}:pending`);
    } else if (child.error !== undefined || !child.doc) {
      parts.push(`${sb.name}:error`);
    } else {
      const wfName = child.doc.workflows?.[0]?.name;
      const nested = expansionKeyRec(
        child.doc,
        resolved,
        docsByPath,
        new Set([...ancestry, resolved]),
        depth + 1,
      );
      parts.push(
        `${sb.name}:${resolved}:${getTopologyKey(child.doc, wfName)}${nested ? `{${nested}}` : ""}`,
      );
    }
  }
  return parts.join(";;");
}

export interface ExpandSubbotsOptions {
  // ROOT-level subbot names to keep compact (e.g. members of a collapsed
  // group — applyGroups must be able to hide them without stranding
  // frame children on a dangling parentId).
  skipRootSubbots?: Set<string>;
}

/** Expands every subbot node whose child document is loaded into a container
 *  frame holding the child graph — recursively for nested subbots. Pure:
 *  returns new node/edge arrays.
 *
 *  - Child ids are prefixed `${subbotId}::` (chained across levels);
 *    children get parentId + extent:"parent" (React Flow compound) and
 *    data.external = true.
 *  - The child's virtual __start__ node and its entry edge are dropped.
 *  - Parent edges INTO the subbot retarget to the child's entry node;
 *    parent edges OUT re-source from the child's `done` node when present
 *    (else stay on the frame). Both rewires apply independently (self-loops).
 *  - A subbot whose child failed to load / cycles / exceeds the depth cap
 *    stays compact (with data.loadError carrying the reason for failures).
 */
export function expandSubbots(
  base: { nodes: Node<NodeData>[]; edges: FlowEdge[] },
  doc: IterDocument,
  docPath: string | null,
  docsByPath: Map<string, SubbotDocEntry>,
  opts?: ExpandSubbotsOptions,
): { nodes: Node[]; edges: FlowEdge[] } {
  const subbots = doc.subbots ?? [];
  if (subbots.length === 0 || docsByPath.size === 0) return base;
  return expandRec(
    base,
    doc,
    docPath,
    docsByPath,
    new Set(docPath ? [docPath] : []),
    0,
    opts?.skipRootSubbots,
  );
}

function expandRec(
  base: { nodes: Node[]; edges: FlowEdge[] },
  doc: IterDocument,
  docPath: string | null,
  docsByPath: Map<string, SubbotDocEntry>,
  ancestry: Set<string>,
  depth: number,
  skipSubbots?: Set<string>,
): { nodes: Node[]; edges: FlowEdge[] } {
  const declByName = new Map((doc.subbots ?? []).map((sb) => [sb.name, sb]));
  if (declByName.size === 0) return base;

  let nodes: Node[] = [...base.nodes];
  let edges: FlowEdge[] = [...base.edges];

  for (const [name, decl] of declByName) {
    if (skipSubbots?.has(name)) continue;
    const compactIdx = nodes.findIndex((n) => n.id === name);
    if (compactIdx === -1) continue; // subbot not on this workflow's canvas
    if (!decl.source) continue; // nothing to load — compact node stays

    const resolved = resolveSubbotSource(docPath, decl.source);

    if (ancestry.has(resolved)) {
      const compact = nodes[compactIdx]!;
      nodes[compactIdx] = {
        ...compact,
        data: {
          ...compact.data,
          loadError: `cycle: ${resolved} is already expanded above`,
          sourcePath: resolved,
        },
      };
      continue;
    }
    if (depth >= MAX_SUBBOT_EXPANSION_DEPTH) continue; // compact, no badge

    const child = docsByPath.get(resolved);
    if (!child || !child.doc) {
      if (child?.error !== undefined) {
        // Load failure: keep the compact node, annotate it for the badge.
        const compact = nodes[compactIdx]!;
        nodes[compactIdx] = {
          ...compact,
          data: { ...compact.data, loadError: child.error, sourcePath: resolved },
        };
      }
      continue; // pending or unresolvable — compact node stays
    }

    const childDoc = child.doc;
    const childWorkflow = childDoc.workflows?.[0];
    // Recurse FIRST (on the child's own un-prefixed graph), then prefix
    // the whole result — inner frames and their children pick up the
    // outer prefix in one pass.
    const childGraph = expandRec(
      documentToGraph(childDoc, childWorkflow?.name) as { nodes: Node[]; edges: FlowEdge[] },
      childDoc,
      resolved,
      docsByPath,
      new Set([...ancestry, resolved]),
      depth + 1,
    );
    const entryId = childWorkflow?.entry;

    const childNodes = childGraph.nodes
      .filter((n) => n.id !== "__start__")
      .map((n) => ({
        ...n,
        id: makeSubbotChildId(name, n.id),
        parentId: n.parentId ? makeSubbotChildId(name, n.parentId) : name,
        extent: "parent" as const,
        ariaLabel: `${(n.data as NodeData).kind} node: ${(n.data as NodeData).label} (part of subbot ${name})`,
        data: {
          ...n.data,
          external: true,
          subbotId: name,
          subbotSource: decl.source ?? "",
        },
      }));

    const childEdges = childGraph.edges
      .filter((e) => e.source !== "__start__")
      .map((e) => ({
        ...e,
        id: makeSubbotChildId(name, e.id),
        source: makeSubbotChildId(name, e.source),
        target: makeSubbotChildId(name, e.target),
      }));

    const hasEntry = entryId != null && childNodes.some((n) => n.id === makeSubbotChildId(name, entryId));
    const hasDone = childNodes.some((n) => n.id === makeSubbotChildId(name, "done"));

    const compact = nodes[compactIdx]!;
    const frame: Node<SubbotFrameData> = {
      id: name,
      type: "subbotFrame",
      position: compact.position,
      style: { width: FRAME_INITIAL_W, height: FRAME_INITIAL_H },
      ariaLabel: `subbot ${name}: child workflow from ${decl.source ?? ""}`,
      // An inner frame (recursion) keeps the compact node's compound
      // parentage — re-parented by the outer level's prefix pass.
      ...(compact.parentId && { parentId: compact.parentId, extent: "parent" as const }),
      data: {
        label: name,
        kind: "subbot",
        color: NODE_COLORS.subbot,
        decl,
        source: decl.source ?? "",
        sourcePath: resolved,
        isolated: decl.isolated ?? false,
        childWorkflowName: childWorkflow?.name ?? "",
      },
    };

    // Frame must precede its children in the array (React Flow requires
    // parents before children); splice both in at the compact node's slot.
    nodes = [...nodes.slice(0, compactIdx), frame, ...childNodes, ...nodes.slice(compactIdx + 1)];

    // Retarget and re-source independently (no early return): a
    // self-loop edge on the subbot (`sub -> sub as retry(n)`) needs
    // BOTH rewires — done back around to the entry.
    edges = edges.map((e) => {
      let next = e;
      if (e.target === name && hasEntry) {
        next = { ...next, target: makeSubbotChildId(name, entryId!) };
      }
      if (e.source === name && hasDone) {
        next = { ...next, source: makeSubbotChildId(name, "done") };
      }
      return next;
    });
    edges = [...edges, ...childEdges];
  }

  return { nodes, edges };
}
