// The studio's twin of `pkg/dsl/workflowfile` — the single source of truth
// for which file extensions iterion recognises as workflow source. Keep the
// list in step with `workflowfile.Extensions`: a file the server parses as a
// workflow and the studio shows as plain text is a file the author edits
// without the language that would have caught their mistake.

/** Accepted workflow file suffixes, including the leading dot. */
export const WORKFLOW_EXTENSIONS = [".bot"] as const;

/** Whether a path names a workflow source file (`main.bot`, `lib/nodes.bot`). */
export function isWorkflowFile(path: string): boolean {
  return WORKFLOW_EXTENSIONS.some((ext) => path.endsWith(ext));
}
