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

/** Apply `rewrite` to every comment of one list. Returning null drops the
 *  comment from the list, so a group the studio dissolves disappears from
 *  the declaration that carried it rather than surviving as a line the
 *  canvas can no longer reach. A list the rewrite did not change keeps its
 *  identity. */
function rewriteCommentList(list: Comment[], rewrite: (comment: Comment) => Comment | null): Comment[] {
  let moved = false;
  const next: Comment[] = [];
  for (const comment of list) {
    const replaced = rewrite(comment);
    if (replaced !== comment) moved = true;
    if (replaced !== null) next.push(replaced);
  }
  return moved ? next : list;
}

/** Apply `rewrite` to every comment of the document, wherever it lives. */
export function mapDocumentComments(
  doc: IterDocument,
  rewrite: (comment: Comment) => Comment | null,
): IterDocument {
  return rewriteCommentLists(doc, (list) => rewriteCommentList(list, rewrite)) as IterDocument;
}

/** What the single reader found in the document. */
export interface DocumentGroups {
  /** One per NAME, the first seen winning — what the canvas draws. */
  groups: GroupAnnotation[];
  /** The annotations the dedupe did NOT draw: a hand-written `.bot` can
   *  declare the same group twice, and `documentComments` surfaces both.
   *  They are the author's lines on carriers the canvas never drew — the
   *  group operations leave them alone (`mapDrawnGroupComment`), so they
   *  must not be invisible either: the canvas names them to the author
   *  (useCanvasLayout's toast) instead of silently absorbing them. */
  dropped: GroupAnnotation[];
}

/** The document's group annotations, from every comment it carries — one per
 *  NAME drawn, the rest of each name reported as `dropped`.
 *
 *  The dedupe belongs here and not in `parseGroups`, which reports what the
 *  text says: this is the single reader every call site goes through, and a
 *  group's name is its identity downstream — `documentToGraph` keys its node
 *  `makeGroupNodeId(g.name)`, so two annotations of one name hand React Flow
 *  two nodes sharing an id and one box stops rendering. `addGroup`'s own
 *  duplicate check cannot cover it: the second declaration is hand-written,
 *  and reading every carrier is what made the two meet. */
export function documentGroups(doc: IterDocument | null | undefined): DocumentGroups {
  const seen = new Set<string>();
  const groups: GroupAnnotation[] = [];
  const dropped: GroupAnnotation[] = [];
  for (const g of parseGroups(documentComments(doc))) {
    if (seen.has(g.name)) {
      dropped.push(g);
    } else {
      seen.add(g.name);
      groups.push(g);
    }
  }
  return { groups, dropped };
}

/** Rewrite — or drop, on a null return — the ONE annotation the canvas
 *  draws for `name`: the first that carries it, in `documentComments`
 *  order. A duplicate of the name is the author's line on a carrier the
 *  canvas never drew, so an operation keyed on the name must not rewrite
 *  or delete it — the author is told about it (the `dropped` of
 *  `documentGroups`), and their text waits for them. Removing the drawn
 *  annotation promotes the duplicate to drawn on the next read, where the
 *  author can see it and answer it. */
export function mapDrawnGroupComment(
  doc: IterDocument,
  name: string,
  rewrite: (comment: Comment, group: GroupAnnotation) => Comment | null,
): IterDocument {
  let done = false;
  const step = (c: Comment): Comment | null => {
    if (done || groupNameFromComment(c) !== name) return c;
    const g = parseGroups([c])[0];
    if (!g) return c;
    done = true;
    return rewrite(c, g);
  };
  // `documentComments` reads the file's own list FIRST, whatever position
  // the `comments` key holds in the document object — so the drawn match
  // is the first match there when there is one, and the walk consults the
  // carriers only when the head did not hold the name.
  const head = doc.comments ? rewriteCommentList(doc.comments, step) : doc.comments;
  const withHead = head === doc.comments ? doc : { ...doc, comments: head };
  return mapDocumentComments(withHead, step);
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
