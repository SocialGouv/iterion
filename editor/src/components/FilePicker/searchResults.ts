// Pure aggregation/dedup helper for the FilePicker's cross-tab search.
//
// When the user types into the search field, we want results from EVERY
// data source that feeds the picker — recents, workspace files, and
// examples — not just the active tab. The component imports
// `buildSearchResults` and renders the unified list; the pure function
// is exported separately so it can be unit-tested without a DOM.
//
// Rules (mirrored in searchResults.test.ts):
//   - Empty / whitespace-only query returns [].
//   - Case-insensitive substring match on the file path / example name.
//   - A path that appears in BOTH recents and files is reported once
//     as a "recent" — recents own the trailing trash button, and we
//     don't want to duplicate the same workflow in the result list.
//   - Stable order: matched recents (preserving recents order, which
//     is most-recent-first), then matched files in the order returned
//     by the API, then matched examples in the order returned by the
//     API.

import type { FileEntry } from "@/api/types";

export type SearchResult =
  | { kind: "recent"; path: string }
  | { kind: "file"; path: string }
  | { kind: "example"; name: string };

export function buildSearchResults(
  query: string,
  recents: readonly string[],
  files: readonly FileEntry[],
  examples: readonly string[],
): SearchResult[] {
  const trimmed = query.trim();
  if (trimmed === "") return [];
  const needle = trimmed.toLowerCase();

  const out: SearchResult[] = [];
  const seenPaths = new Set<string>();

  for (const path of recents) {
    if (path.toLowerCase().includes(needle)) {
      out.push({ kind: "recent", path });
      seenPaths.add(path);
    }
  }

  for (const f of files) {
    if (seenPaths.has(f.name)) continue;
    if (f.name.toLowerCase().includes(needle)) {
      out.push({ kind: "file", path: f.name });
      seenPaths.add(f.name);
    }
  }

  for (const name of examples) {
    if (name.toLowerCase().includes(needle)) {
      out.push({ kind: "example", name });
    }
  }

  return out;
}
