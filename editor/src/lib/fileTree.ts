// Build a Git-Graph-style tree from a flat list of changed files.
//
// The renderer in FilesPanel walks this structure recursively. The two
// non-trivial parts:
//
//   1. **Compaction**: directories with exactly one folder child are
//      merged into a single breadcrumb row (".github / workflows")
//      mirroring VS Code's "Compact Folders" behaviour. Pure-file
//      single-children are left alone — only folder→folder chains
//      compact, otherwise the file would lose its indentation level.
//
//   2. **Aggregates**: each folder caches the sum of added/deleted
//      counts of its descendants so a collapsed folder can still show
//      "+N | -N" without the children being mounted. Binary descendants
//      are excluded from the sum but tracked via hasBinaryDescendant
//      so callers can decide whether to flag the aggregate as partial.

import type { RunFile } from "@/api/runs";

export type TreeNode = TreeFolder | TreeFile;

export interface TreeFolder {
  kind: "folder";
  /** Display label, possibly multi-segment after compaction. */
  label: string;
  /** Slash-joined original path of this folder; stable React key. */
  pathKey: string;
  /** Depth in the *post-compaction* tree (top-level = 0). */
  depth: number;
  children: TreeNode[];
  /** Sum of children's added counts (binary descendants excluded). */
  added: number;
  deleted: number;
  hasBinaryDescendant: boolean;
}

export interface TreeFile {
  kind: "file";
  file: RunFile;
  /** basename only, e.g. "release.yml". */
  label: string;
  /** Full path == file.path. */
  pathKey: string;
  depth: number;
}

interface MutFolder {
  kind: "folder";
  label: string;
  pathKey: string;
  // We use a Map for child folders to keep insertion O(1) and avoid
  // repeated linear scans during tree construction. Files are stored
  // in a parallel array — they're terminal so they never need lookup.
  childFolders: Map<string, MutFolder>;
  childFiles: TreeFile[];
  added: number;
  deleted: number;
  hasBinaryDescendant: boolean;
}

export function buildFileTree(files: RunFile[]): TreeNode[] {
  // Synthetic root holds the top-level entries; never surfaced.
  const root: MutFolder = newFolder("", "");

  for (const file of files) {
    const segments = file.path.split("/");
    if (segments.length === 1) {
      root.childFiles.push({
        kind: "file",
        file,
        label: segments[0]!,
        pathKey: file.path,
        depth: 0,
      });
      continue;
    }
    let cursor = root;
    for (let i = 0; i < segments.length - 1; i++) {
      const seg = segments[i]!;
      const path = cursor.pathKey === "" ? seg : `${cursor.pathKey}/${seg}`;
      let child = cursor.childFolders.get(seg);
      if (!child) {
        child = newFolder(seg, path);
        cursor.childFolders.set(seg, child);
      }
      cursor = child;
    }
    cursor.childFiles.push({
      kind: "file",
      file,
      label: segments[segments.length - 1]!,
      pathKey: file.path,
      depth: 0,
    });
  }


  // Sort, aggregate counts, then compact single-child folder chains.
  // Depths are written in a final pass so they reflect the
  // post-compaction shape (each compacted breadcrumb counts as one
  // indentation step, not the number of segments it consumed).
  const finalised = finaliseChildren(root);
  return assignDepths(finalised.children);
}

function newFolder(label: string, pathKey: string): MutFolder {
  return {
    kind: "folder",
    label,
    pathKey,
    childFolders: new Map(),
    childFiles: [],
    added: 0,
    deleted: 0,
    hasBinaryDescendant: false,
  };
}

// finaliseChildren returns an immutable TreeFolder reflecting the same
// content as `mut`, with children sorted (folders before files, alpha
// within group, "??" pushed to end of files), recursively finalised,
// counts aggregated, and folder→folder chains compacted into single
// rows. The wrapper TreeFolder has no depth assigned yet — that's the
// final pass after compaction so depth reflects the post-compaction
// shape, not the raw tree.
function finaliseChildren(mut: MutFolder): TreeFolder {
  // Recurse first so we have aggregates ready when we compact.
  const folderChildren: TreeFolder[] = [];
  for (const child of mut.childFolders.values()) {
    folderChildren.push(finaliseChildren(child));
  }
  folderChildren.sort((a, b) => a.label.localeCompare(b.label));

  const fileChildren = [...mut.childFiles].sort(compareFiles);

  let added = 0;
  let deleted = 0;
  let hasBinary = false;
  for (const f of folderChildren) {
    added += f.added;
    deleted += f.deleted;
    hasBinary ||= f.hasBinaryDescendant;
  }
  for (const f of fileChildren) {
    if (f.file.binary || f.file.added < 0 || f.file.deleted < 0) {
      hasBinary = true;
      continue;
    }
    added += f.file.added;
    deleted += f.file.deleted;
  }

  let folder: TreeFolder = {
    kind: "folder",
    label: mut.label,
    pathKey: mut.pathKey,
    depth: 0,
    children: [...folderChildren, ...fileChildren],
    added,
    deleted,
    hasBinaryDescendant: hasBinary,
  };

  // Compaction: a folder whose only child is a folder gets merged.
  // We *don't* compact the synthetic root (label === "") — its
  // children are the displayed top-level rows.
  if (folder.label !== "") {
    while (
      folder.children.length === 1 &&
      folder.children[0]!.kind === "folder"
    ) {
      const only = folder.children[0] as TreeFolder;
      folder = {
        ...folder,
        // Use ASCII " / " as the visual separator so the row reads
        // unambiguously even when a segment itself contains spaces.
        label: `${folder.label} / ${only.label}`,
        children: only.children,
        // Aggregates and pathKey are the parent's — pathKey stays
        // anchored to the topmost segment so React keys remain stable
        // across renders (and so the collapse-state Set keeps tracking
        // the same logical row even if more files arrive deeper down).
      };
    }
  }

  return folder;
}

function compareFiles(a: TreeFile, b: TreeFile): number {
  const ua = a.file.status === "??" ? 1 : 0;
  const ub = b.file.status === "??" ? 1 : 0;
  if (ua !== ub) return ua - ub;
  return a.label.localeCompare(b.label);
}

// Walk the tree top-down and write the post-compaction depth onto each
// node. Done as a separate pass so a compacted breadcrumb counts as
// one indentation step regardless of how many original segments it
// consumed.
function assignDepths(nodes: TreeNode[], depth = 0): TreeNode[] {
  for (const node of nodes) {
    node.depth = depth;
    if (node.kind === "folder") {
      assignDepths(node.children, depth + 1);
    }
  }
  return nodes;
}
