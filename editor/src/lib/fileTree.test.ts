import { describe, expect, it } from "vitest";

import type { RunFile } from "@/api/runs";
import { buildFileTree, type TreeFile, type TreeFolder } from "./fileTree";

function file(
  path: string,
  overrides: Partial<RunFile> = {},
): RunFile {
  return {
    path,
    status: "M",
    added: 0,
    deleted: 0,
    ...overrides,
  };
}

describe("buildFileTree", () => {
  it("returns empty array for empty input", () => {
    expect(buildFileTree([])).toEqual([]);
  });

  it("compacts single-child folder chains into a breadcrumb", () => {
    const tree = buildFileTree([
      file(".github/workflows/release.yml", { status: "A", added: 35 }),
    ]);
    expect(tree).toHaveLength(1);
    const top = tree[0]! as TreeFolder;
    expect(top.kind).toBe("folder");
    expect(top.label).toBe(".github / workflows");
    expect(top.depth).toBe(0);
    expect(top.children).toHaveLength(1);
    const leaf = top.children[0]! as TreeFile;
    expect(leaf.kind).toBe("file");
    expect(leaf.label).toBe("release.yml");
    expect(leaf.depth).toBe(1);
  });

  it("does not compact a folder with multiple children", () => {
    const tree = buildFileTree([
      file("a/b/x.txt"),
      file("a/c/y.txt"),
    ]);
    expect(tree).toHaveLength(1);
    const a = tree[0]! as TreeFolder;
    expect(a.label).toBe("a"); // not compacted: a has two folder children
    expect(a.children).toHaveLength(2);
  });

  it("sorts folders before files and untracked last among files", () => {
    const tree = buildFileTree([
      file("z.txt", { status: "??" }),
      file("a.txt"),
      file("dir/inner.txt"),
    ]);
    // dir (folder) first, then a.txt, then z.txt (?? pushed to end).
    expect(tree.map((n) => n.label)).toEqual(["dir", "a.txt", "z.txt"]);
  });

  it("aggregates added/deleted counts up the tree", () => {
    const tree = buildFileTree([
      file("src/a.go", { added: 3, deleted: 1 }),
      file("src/sub/b.go", { added: 2, deleted: 4 }),
    ]);
    const src = tree[0]! as TreeFolder;
    expect(src.label).toBe("src");
    expect(src.added).toBe(5);
    expect(src.deleted).toBe(5);
    expect(src.hasBinaryDescendant).toBe(false);
  });

  it("flags binary descendants and excludes them from the sum", () => {
    const tree = buildFileTree([
      file("assets/logo.png", { added: -1, deleted: -1, binary: true }),
      file("assets/copy.css", { added: 4, deleted: 0 }),
    ]);
    const folder = tree[0]! as TreeFolder;
    expect(folder.added).toBe(4);
    expect(folder.deleted).toBe(0);
    expect(folder.hasBinaryDescendant).toBe(true);
  });

  it("places top-level files after top-level folders", () => {
    const tree = buildFileTree([
      file("README.md"),
      file("src/main.ts"),
    ]);
    expect(tree.map((n) => n.label)).toEqual(["src", "README.md"]);
    expect(tree[0]!.depth).toBe(0);
    expect(tree[1]!.depth).toBe(0);
  });

  it("preserves rename old_path on the file row", () => {
    const tree = buildFileTree([
      file("dir/new.go", { status: "R", old_path: "dir/old.go" }),
    ]);
    const dir = tree[0]! as TreeFolder;
    const leaf = dir.children[0]! as TreeFile;
    expect(leaf.file.old_path).toBe("dir/old.go");
  });

  it("compacts multi-step folder chains into a single label", () => {
    // a/b/c/leaf.txt → "a / b / c" + leaf.txt
    const tree = buildFileTree([file("a/b/c/leaf.txt")]);
    expect(tree).toHaveLength(1);
    const top = tree[0]! as TreeFolder;
    expect(top.label).toBe("a / b / c");
    expect(top.depth).toBe(0);
    expect((top.children[0] as TreeFile).depth).toBe(1);
  });
});
