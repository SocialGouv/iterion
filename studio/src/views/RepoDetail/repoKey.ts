import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";

// URL codec for the connected-repo key (`<connection_id>::<owner/repo>`)
// used by the /repos/:key route. The key contains slashes (repo full
// name) and colons, so it must travel as ONE encoded path segment:
// encodeURIComponent turns `/` into %2F and `:` into %3A, both of which
// wouter's path unescape (decodeURI) leaves intact — the route matcher
// sees a single segment and hands the still-encoded param back.

export function encodeRepoKey(key: string): string {
  return encodeURIComponent(key);
}

/** Inverse of encodeRepoKey; a malformed escape returns the raw param
 *  so a hand-mangled URL degrades to "repo not found", not a crash. */
export function decodeRepoKey(param: string): string {
  try {
    return decodeURIComponent(param);
  } catch {
    return param;
  }
}

/** The repo detail route for a connected repo row. */
export function repoDetailPath(
  r: Pick<ForgeTeamRepo, "connection_id" | "repo_full_name">,
): string {
  return `/repos/${encodeRepoKey(forgeTeamRepoKey(r))}`;
}
