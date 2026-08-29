export interface SharedBundleFilePath {
  name: string;
  relativePath: string;
}

/** Parse only the project-root materialisation owned by `iterion bots sync`. */
export function parseSharedBundleFilePath(
  path: string | null,
): SharedBundleFilePath | null {
  if (!path) return null;
  const normalised = path.replace(/\\/g, "/").replace(/^\.\//, "");
  const parts = normalised.split("/");
  if (parts.length < 3 || parts[0] !== ".botz" || !parts[1]) return null;
  return { name: parts[1], relativePath: parts.slice(2).join("/") };
}

export function isSharedBundleFilePath(path: string | null): boolean {
  return parseSharedBundleFilePath(path) !== null;
}
