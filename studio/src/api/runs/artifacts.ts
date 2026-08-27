// Extracted from api/runs.ts to keep that file focused.
// Read-side artifact endpoints: workflow IR projection, versioned per-node
// artifacts (JSON), and the artifact-files area (binary/text files written
// by in-sandbox tools via $ITERION_ARTIFACT_FILES_DIR).

import { desktop, isDesktop } from "@/lib/desktopBridge";
import { downloadBlob } from "@/lib/download";
import { apiURL, extractErrorMessage, request, withStoreParam } from "./client";
import type {
  Artifact,
  ArtifactFile,
  ArtifactSummary,
  DownloadOutcome,
  PlanSnapshot,
  RunArtifactSummary,
  WireWorkflow,
} from "./types";

// listAllArtifacts returns the latest published artifact per node for a
// run (node id, version, labels, title) — the data behind the centralized,
// label-grouped Artifacts view. Empty when the run published nothing.
export async function listAllArtifacts(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<RunArtifactSummary[]> {
  const res = await request<{ artifacts: RunArtifactSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/artifacts`,
    { signal: opts?.signal },
  );
  return res.artifacts ?? [];
}

export async function getRunWorkflow(runId: string): Promise<WireWorkflow> {
  const qs = withStoreParam(new URLSearchParams()).toString();
  return request(`/runs/${encodeURIComponent(runId)}/workflow${qs ? `?${qs}` : ""}`);
}

export async function listArtifacts(
  runId: string,
  nodeId: string,
  opts?: { signal?: AbortSignal },
): Promise<ArtifactSummary[]> {
  const res = await request<{ artifacts: ArtifactSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(nodeId)}`,
    { signal: opts?.signal },
  );
  return res.artifacts ?? [];
}

export async function getArtifact(
  runId: string,
  nodeId: string,
  version: number,
  opts?: { signal?: AbortSignal },
): Promise<Artifact> {
  return request(
    `/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(nodeId)}/${version}`,
    { signal: opts?.signal },
  );
}

// listPlans returns the chronological plan snapshots captured for a run —
// the agents' TodoWrite/todo_write living TODO lists. Ascending seq order
// (how the plan evolved). Empty for older runs / cloud stores that predate
// or don't back the feature.
export async function listPlans(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<PlanSnapshot[]> {
  const res = await request<{ plans: PlanSnapshot[] }>(
    `/runs/${encodeURIComponent(runId)}/plans`,
    { signal: opts?.signal },
  );
  return res.plans ?? [];
}

export async function listArtifactFiles(runId: string): Promise<ArtifactFile[]> {
  const res = await request<{ files: ArtifactFile[] }>(
    `/runs/${encodeURIComponent(runId)}/artifact-files`,
  );
  return res.files ?? [];
}

// Build the URL to download a single artifact file. Returns a string
// (not a fetch wrapper) because the caller hands it straight to an
// `<a href>` for browser download / new-tab preview.
export function artifactFileURL(runId: string, relPath: string): string {
  // The path can contain `/` segments; encodeURIComponent would clobber
  // them. Encode each segment individually so subdirs survive.
  const segments = relPath.split("/").map(encodeURIComponent).join("/");
  return apiURL(`/runs/${encodeURIComponent(runId)}/artifact-files/${segments}`);
}

// fetchArtifactFile downloads one artifact file body via the same
// auth-aware fetch surface as every other API call (cookies + Bearer).
// `download=true` flips the backend's Content-Disposition to
// `attachment` so previewable content types (json, md) still trigger
// a real download instead of an inline render.
export async function fetchArtifactFile(
  runId: string,
  relPath: string,
  opts: { download?: boolean } = {},
): Promise<{ blob: Blob; contentType: string }> {
  const url = artifactFileURL(runId, relPath) + (opts.download ? "?download=1" : "");
  const res = await fetch(url, { credentials: "include" });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await extractErrorMessage(res)}`);
  }
  return {
    blob: await res.blob(),
    contentType: res.headers.get("Content-Type") ?? "application/octet-stream",
  };
}

// downloadArtifactFile fetches the file and saves it to disk. In
// desktop mode (Wails) it routes through the SaveBinaryFile native
// binding so a real save dialog opens — the embedded WebKit silently
// swallows `<a download>` blob URLs, which is why this can't just
// rely on the DOM trick. In browser mode we fall back to the blob
// URL approach, which the user's browser handles natively.
export async function downloadArtifactFile(
  runId: string,
  relPath: string,
): Promise<DownloadOutcome> {
  const { blob, contentType } = await fetchArtifactFile(runId, relPath, { download: true });
  const basename = relPath.includes("/")
    ? relPath.slice(relPath.lastIndexOf("/") + 1)
    : relPath;

  if (isDesktop()) {
    const b64 = await blobToBase64(blob);
    const localPath = await desktop.saveBinaryFile(basename, b64);
    if (!localPath) {
      // User cancelled the native save dialog.
      return { cancelled: true, contentType };
    }
    return { cancelled: false, localPath, contentType };
  }

  downloadBlob(blob, basename, { revokeMs: 5000 });
  return { cancelled: false, contentType };
}

// blobToBase64 strips the `data:<mime>;base64,` prefix from FileReader
// output and hands back the raw payload — the Wails SaveBinaryFile
// binding decodes plain base64 (no data-URL wrapper).
async function blobToBase64(blob: Blob): Promise<string> {
  const buf = await blob.arrayBuffer();
  const bytes = new Uint8Array(buf);
  // Build the binary-string in 32 KB pieces and join at the end:
  // `binary += String.fromCharCode(...)` would be O(N²) on the total
  // size (every += copies an ever-growing immutable string), freezing
  // the UI thread for several seconds on multi-MB artifact downloads.
  // Array#join allocates once over the concatenated length.
  const parts: string[] = [];
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    parts.push(String.fromCharCode(...bytes.subarray(i, i + chunk)));
  }
  return btoa(parts.join(""));
}

// A conversational bot can produce a `.bot` DRAFT as part of a turn — Copi's
// `design` posture does, and a deterministic node compiles it before the
// operator reads the reply. The draft is NOT a file: the bot is read-only with
// respect to the workspace, so it lives only in that node's artifact.
//
// Found by SHAPE, never by node name. The studio's chat registry is
// manifest-driven (issue #333), so hardcoding "copi" here would put a specific
// bot's node id back into studio code — the same coupling the registry
// removed. Any node publishing a non-empty `draft_bot` string qualifies.
export const DRAFT_BOT_FIELD = "draft_bot";

// The bot's own posture for the turn. A conversational bot that designs
// workflows announces it here BEFORE it has anything to show, which is what
// lets the studio offer the editor first and the draft second — the order the
// operator asked for: settle where the work happens, then do it there.
export const MODE_FIELD = "mode";
export const DESIGN_MODE = "design";

export interface DraftLookup {
  /** The `.bot` source, when this conversation has produced one. */
  source: string | null;
  /** True when the latest turn is designing, draft or not. */
  designing: boolean;
}

// Newest artifacts first: a draft is the LATEST thing the conversation
// produced, so the last-written node is where the search should start.
export async function lookupDraft(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<DraftLookup> {
  const summaries = await listAllArtifacts(runId, opts);
  const ordered = [...summaries].sort((a, b) =>
    (b.written_at ?? "").localeCompare(a.written_at ?? ""),
  );
  let designing = false;
  for (const s of ordered) {
    if (opts?.signal?.aborted) return { source: null, designing };
    let art: Artifact;
    try {
      art = await getArtifact(runId, s.node_id, s.version, opts);
    } catch {
      // One unreadable artifact must not hide a draft published by another
      // node — keep looking rather than failing the whole lookup.
      continue;
    }
    // The NEWEST posture wins, so read it from the first artifact that
    // declares one rather than letting an older turn's mode linger.
    if (!designing && art.data?.[MODE_FIELD] === DESIGN_MODE) designing = true;
    const value = art.data?.[DRAFT_BOT_FIELD];
    if (typeof value === "string" && value.trim() !== "") {
      return { source: value, designing: true };
    }
  }
  return { source: null, designing };
}

/** Back-compat shorthand for callers that only want the source. */
export async function findDraftBotSource(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<string | null> {
  return (await lookupDraft(runId, opts)).source;
}
