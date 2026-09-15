import type { UnitInfo } from "@/api/types";

/** touchesOpenUnit reports whether a change to `eventPath` is a change to
 *  the open document: the file itself, or — for a bot in several files —
 *  one of the fragments its imports reach, whose text and revision the
 *  merged document carries. Paths are compared as the server names them:
 *  the unit's files sit under its root, the root being the open file's
 *  directory as it was named ("" for a cloud bundle, whose files are
 *  keyed from the bundle's root). */
export function touchesOpenUnit(eventPath: string, filePath: string | null, unit: UnitInfo | null): boolean {
  if (!filePath) return false;
  if (eventPath === filePath) return true;
  if (!unit) return false;
  return unit.files.some((f) => unitFilePath(unit.root, f.rel) === eventPath);
}

/** unitFilePath is a unit file's path as the watcher names it. */
export function unitFilePath(root: string, rel: string): string {
  return root && root !== "." ? `${root}/${rel}` : rel;
}
