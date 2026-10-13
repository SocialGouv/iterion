import type { UnitInfo } from "@/api/types";

/** touchesOpenUnit reports whether a change to `eventPath` is a change to
 *  the open document: the file itself, or — for a bot in several files —
 *  one of the fragments its imports reach, whose text and revision the
 *  merged document carries. Paths are compared as the server names them:
 *  the unit's files sit under its root, which composes with any of its rels
 *  — the main's included — into the path that was opened. That root is ""
 *  wherever none can compose them: a cloud bundle, whose files are keyed
 *  from the bundle's root, and a unit the server read from ABOVE the
 *  workspace (unitRootOf), whose rels then name no path under it and leave
 *  this answering for the open file alone. */
export function touchesOpenUnit(eventPath: string, filePath: string | null, unit: UnitInfo | null): boolean {
  if (!filePath) return false;
  if (eventPath === filePath) return true;
  if (!unit) return false;
  return unit.files.some((f) => unitFilePath(unit.root, f.rel) === eventPath);
}

/** unitAwaitsFragment reports whether `eventPath` names a fragment the open
 *  unit's load tried to read and could not. Such a file is a file of NO
 *  list: the load holds every file it reached and nothing of the one it did
 *  not — while the launch refuses on that load, and so does the gate. So
 *  membership cannot recognise the author writing the fragment the refusal
 *  names, which is the repair the refusal asks for.
 *
 *  It is read from `unread`, which the server answers beside the verdict it
 *  is about, never re-derived from the file list's `imports`: an apply that
 *  drops the broken `import` line answers the STORED verdict over the
 *  STAGED list, where no file names that fragment any more — and an import
 *  the loader refuses by PATH, outside the bot's `lib/`, names a file no
 *  write makes readable. */
export function unitAwaitsFragment(eventPath: string, unit: UnitInfo | null): boolean {
  if (!unit) return false;
  return (unit.unread ?? []).some((rel) => unitFilePath(unit.root, rel) === eventPath);
}

/** unitFilePath is a unit file's path as the watcher names it. */
export function unitFilePath(root: string, rel: string): string {
  return root && root !== "." ? `${root}/${rel}` : rel;
}
