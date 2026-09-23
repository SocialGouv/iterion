import type { Comment, IterDocument } from "@/api/types";

/** A visual group annotation parsed from structured comments. */
export interface GroupAnnotation {
  name: string;
  nodeIds: string[];
}

const GROUP_PREFIX = "@group ";

// ---------------------------------------------------------------------------
// Where a comment lives
// ---------------------------------------------------------------------------
//
// `document.comments` is the file's head and tail only. Since comment
// provenance (#1282) a comment written around a declaration is carried BY
// that declaration (`document.agents[i].comments`), and one written on an
// edge by that edge (`workflows[i].edges[j].comments`) — so a reader that
// takes `document.comments` sees an arbitrary subset of what the file says.
// The two functions below are the single place that knows where comments
// live: `documentComments` reads them all, `mapDocumentComments` rewrites
// them where they are. They share one walk, so a carrier one of them reaches
// is a carrier the other reaches.

/** A `comments` array holds Comment objects at every carrier the document
 *  type declares — the marker is the key, so a declaration kind added later
 *  is covered the day its JSON arrives, without a list to keep in step.
 *
 *  The test is only that the elements are objects. Keying it on `text` being
 *  a string was wrong in the direction that loses data: `Comment.Text` is
 *  `omitempty` on the wire (pkg/dsl/ast/jsonenc.go), so a bare `##` — the
 *  paragraph break every long head block uses, 8 of them in
 *  bots/feature-dev/main.bot alone — arrives as `{}`, and one of those in a
 *  list made `every()` false for the WHOLE list. Every group in it then
 *  disappeared from the canvas and no rewrite could reach it. */
function isCommentList(value: unknown): value is Comment[] {
  return Array.isArray(value) && value.every((c) => typeof c === "object" && c !== null);
}

function isWalkable(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Visit every `comments` array in the document tree, in document order. */
function walkCommentLists(value: unknown, visit: (list: Comment[]) => void): void {
  if (Array.isArray(value)) {
    for (const item of value) walkCommentLists(item, visit);
    return;
  }
  if (!isWalkable(value)) return;
  for (const [key, child] of Object.entries(value)) {
    if (key === "comments" && isCommentList(child)) {
      visit(child);
      continue;
    }
    walkCommentLists(child, visit);
  }
}

/** Rewrite every `comments` array in the document tree. Branches the
 *  rewrite did not change keep their identity, so a document with no group
 *  comment to touch comes back as the same object and the canvas does not
 *  re-render for nothing. */
function rewriteCommentLists(value: unknown, rewrite: (list: Comment[]) => Comment[]): unknown {
  if (Array.isArray(value)) {
    let moved = false;
    const next = value.map((item) => {
      const replaced = rewriteCommentLists(item, rewrite);
      if (replaced !== item) moved = true;
      return replaced;
    });
    return moved ? next : value;
  }
  if (!isWalkable(value)) return value;
  let moved = false;
  const next: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(value)) {
    if (key === "comments" && isCommentList(child)) {
      const list = rewrite(child);
      next[key] = list;
      if (list !== child) moved = true;
      continue;
    }
    const replaced = rewriteCommentLists(child, rewrite);
    next[key] = replaced;
    if (replaced !== child) moved = true;
  }
  return moved ? next : value;
}

/** Every `##` comment the document carries, wherever it was written: the
 *  file's head and tail, and the ones a declaration or an edge holds. This
 *  is what the group grammar reads — an annotation is a group whether its
 *  author wrote it above the first declaration or above the `dsl:` header. */
export function documentComments(doc: IterDocument | null | undefined): Comment[] {
  if (!doc) return [];
  // The file's own list first, then every carrier in document order. A
  // stated rule, not the accident of a key order: `documentGroups` breaks a
  // duplicate name on the FIRST seen, and `document.comments` is where the
  // studio writes the groups it creates, so a hand-written duplicate loses
  // to the one the canvas drew.
  const all: Comment[] = [...(doc.comments ?? [])];
  walkCommentLists(doc, (list) => {
    if (list === doc.comments) return;
    all.push(...list);
  });
  return all;
}

/** Apply `rewrite` to every comment of the document, wherever it lives.
 *  Returning null drops the comment from the list that holds it, so a group
 *  the studio dissolves disappears from the declaration that carried it
 *  rather than surviving as a line the canvas can no longer reach. */
export function mapDocumentComments(
  doc: IterDocument,
  rewrite: (comment: Comment) => Comment | null,
): IterDocument {
  return rewriteCommentLists(doc, (list) => {
    let moved = false;
    const next: Comment[] = [];
    for (const comment of list) {
      const replaced = rewrite(comment);
      if (replaced !== comment) moved = true;
      if (replaced !== null) next.push(replaced);
    }
    return moved ? next : list;
  }) as IterDocument;
}

/** The document's group annotations, from every comment it carries — one per
 *  NAME, the first seen winning.
 *
 *  The dedupe belongs here and not in `parseGroups`, which reports what the
 *  text says: this is the single reader every call site goes through, and a
 *  group's name is its identity downstream — `documentToGraph` keys its node
 *  `makeGroupNodeId(g.name)`, so two annotations of one name hand React Flow
 *  two nodes sharing an id and one box stops rendering. `addGroup`'s own
 *  duplicate check cannot cover it: the second declaration is hand-written,
 *  and reading every carrier is what made the two meet.
 *
 *  What this does NOT close, stated so the next reader does not assume it
 *  does: `removeGroup` and `updateGroup` key on the NAME through
 *  `mapDocumentComments`, so they still reach the annotation this reader
 *  dropped — one canvas drag rewrites a line the author wrote on a
 *  declaration the canvas never drew, and nothing names the duplicate to
 *  them. That is older than this reader and orthogonal to the render bug it
 *  fixes; it needs a surface, not a wider predicate. */
export function documentGroups(doc: IterDocument | null | undefined): GroupAnnotation[] {
  const seen = new Set<string>();
  return parseGroups(documentComments(doc)).filter((g) => {
    if (seen.has(g.name)) return false;
    seen.add(g.name);
    return true;
  });
}

/** Parse @group annotations from document comments.
 *  Format: `@group <name>: node1, node2, node3` */
export function parseGroups(comments: Comment[]): GroupAnnotation[] {
  const groups: GroupAnnotation[] = [];
  for (const c of comments) {
    if (!c.text) continue;
    const text = c.text.trim();
    if (!text.startsWith(GROUP_PREFIX)) continue;
    const rest = text.slice(GROUP_PREFIX.length);
    const colonIdx = rest.indexOf(":");
    if (colonIdx === -1) continue;
    const name = rest.slice(0, colonIdx).trim();
    const nodesStr = rest.slice(colonIdx + 1).trim();
    if (!name || !nodesStr) continue;
    const nodeIds = nodesStr.split(",").map((s) => s.trim()).filter(Boolean);
    if (nodeIds.length > 0) {
      groups.push({ name, nodeIds });
    }
  }
  return groups;
}

/** Serialize a group annotation back to a comment string (without ## prefix — that's added by unparse). */
export function groupToCommentText(group: GroupAnnotation): string {
  return `${GROUP_PREFIX}${group.name}: ${group.nodeIds.join(", ")}`;
}


/** Extract group name from a single comment, or null if not a group comment. */
export function groupNameFromComment(comment: Comment): string | null {
  if (!comment.text) return null;
  const text = comment.text.trim();
  if (!text.startsWith(GROUP_PREFIX)) return null;
  const rest = text.slice(GROUP_PREFIX.length);
  const colonIdx = rest.indexOf(":");
  if (colonIdx === -1) return null;
  const name = rest.slice(0, colonIdx).trim();
  return name || null;
}

/** Group node ID prefix for XYFlow. */
const GROUP_PREFIX_ID = "__group__:";

export function makeGroupNodeId(groupName: string): string {
  return `${GROUP_PREFIX_ID}${groupName}`;
}

export function isGroupNodeId(id: string): boolean {
  return id.startsWith(GROUP_PREFIX_ID);
}

export function groupNameFromNodeId(id: string): string {
  return id.slice(GROUP_PREFIX_ID.length);
}
