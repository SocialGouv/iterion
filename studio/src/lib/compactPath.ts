// compactPath shortens an absolute filesystem path for table cells:
// long paths dominate a row with noise while only the tail carries
// signal. Keeps the last `keep` segments prefixed with "…/"; short
// paths pass through untouched. Pair with title={fullPath} so the
// full value stays one hover away.
export function compactPath(path: string, keep = 2): string {
  const segments = path.split("/").filter(Boolean);
  if (segments.length <= keep + 1) return path;
  return `…/${segments.slice(-keep).join("/")}`;
}
