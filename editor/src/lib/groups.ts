import type { Comment } from "@/api/types";

/** A visual group annotation parsed from structured comments. */
export interface GroupAnnotation {
  name: string;
  nodeIds: string[];
}

const GROUP_PREFIX = "@group ";

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

/** Check if a comment is a group annotation. */
export function isGroupComment(comment: Comment): boolean {
  return !!comment.text && comment.text.trim().startsWith(GROUP_PREFIX);
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
export const GROUP_PREFIX_ID = "__group__:";

export function makeGroupNodeId(groupName: string): string {
  return `${GROUP_PREFIX_ID}${groupName}`;
}

export function isGroupNodeId(id: string): boolean {
  return id.startsWith(GROUP_PREFIX_ID);
}

export function groupNameFromNodeId(id: string): string {
  return id.slice(GROUP_PREFIX_ID.length);
}
