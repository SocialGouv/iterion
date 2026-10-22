// A run's artifact file has one shareable address: the run page itself,
// asked to open that file — `/runs/<id>?file=<path>`. The run view reads
// the `file` query key, opens the Artifacts drawer, and the files panel
// shows the file in its preview (rendered when it is markdown). Keeping
// the link on the run route means the reader lands with the run's
// context (team, status, other deliverables) and never on a raw API body.
//
// The run route carries other query keys that must survive a file link —
// `?store=<path>` points a run at a foreign store and every fetch reads it
// from the address bar — so paths are built FROM the current search, never
// from scratch.

export const ARTIFACT_FILE_PARAM = "file";

function searchParams(search: string): URLSearchParams {
  return new URLSearchParams(search.startsWith("?") ? search.slice(1) : search);
}

function runPath(runId: string, params: URLSearchParams): string {
  const qs = params.toString();
  return `/runs/${encodeURIComponent(runId)}${qs ? `?${qs}` : ""}`;
}

// artifactFileFromSearch reads the file a run URL asks to open, or null
// when the query carries none (or an empty one).
export function artifactFileFromSearch(search: string): string | null {
  const value = searchParams(search).get(ARTIFACT_FILE_PARAM);
  return value && value.trim() !== "" ? value : null;
}

// artifactFileRunPath builds the base-relative run route that opens one
// artifact file, keeping every other key of `baseSearch` (the current
// query). The path is encoded as a single query value so `/` in a
// subdirectory path survives the round trip through URLSearchParams.
export function artifactFileRunPath(runId: string, relPath: string, baseSearch = ""): string {
  const params = searchParams(baseSearch);
  params.set(ARTIFACT_FILE_PARAM, relPath);
  return runPath(runId, params);
}

// runPathWithoutFile is the run route with the `file` key removed and
// every other key of `baseSearch` kept — where closing a preview lands.
export function runPathWithoutFile(runId: string, baseSearch = ""): string {
  const params = searchParams(baseSearch);
  params.delete(ARTIFACT_FILE_PARAM);
  return runPath(runId, params);
}

// isMarkdownType says whether a preview body should be rendered as
// markdown rather than shown as source. Matches the artifact-files
// endpoint's `text/markdown; charset=utf-8` and the bare type.
export function isMarkdownType(contentType: string): boolean {
  return /^text\/markdown(\s*;|$)/i.test(contentType.trim());
}
