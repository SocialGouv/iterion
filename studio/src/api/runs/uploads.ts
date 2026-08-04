// Extracted from api/runs.ts to keep that file focused.
// Server info + attachment staging: getServerInfo for the BackendStatusPill /
// LaunchView, uploadAttachment for the staged-upload XHR (with progress),
// and read-back of a promoted run attachment (gate payload previews).

import type { ServerInfo, StagedUpload } from "../types";
import { apiURL, BASE_URL, extractErrorMessage, request } from "./client";
import type { UploadOptions } from "./types";

/** GET /api/server/info — mode, version, upload limits. */
export async function getServerInfo(): Promise<ServerInfo> {
  return request("/server/info");
}

/**
 * URL of one promoted run attachment (GET /api/runs/{id}/attachments/{name}).
 *
 * Distinct from artifactFileURL: an attachment is addressed by its
 * declared NAME, not by a workspace-relative path — the path a file
 * descriptor carries points at the host (or the sandbox bind-mount) and
 * is not reachable from a browser at all.
 */
export function attachmentURL(runId: string, name: string): string {
  return apiURL(
    `/runs/${encodeURIComponent(runId)}/attachments/${encodeURIComponent(name)}`,
  );
}

/**
 * Fetch an attachment's bytes through the auth-aware surface (cookies +
 * Bearer), for inline preview. The server sends the sniffed MIME, which
 * is authoritative over any client-side guess from the filename.
 */
export async function fetchAttachment(
  runId: string,
  name: string,
): Promise<{ blob: Blob; contentType: string }> {
  const res = await fetch(attachmentURL(runId, name), { credentials: "include" });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await extractErrorMessage(res)}`);
  }
  return {
    blob: await res.blob(),
    contentType: res.headers.get("Content-Type") ?? "application/octet-stream",
  };
}

/**
 * POST /api/runs/uploads — upload a single attachment to the server's
 * staging area. Uses XMLHttpRequest because fetch() in browsers does
 * not yet expose request-side upload progress (ReadableStream upload
 * is half-duplex and Chromium-only).
 */
export function uploadAttachment(
  file: File,
  opts: UploadOptions = {},
): Promise<StagedUpload> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const fd = new FormData();
    fd.append("file", file, file.name);
    if (opts.declaredMime) fd.append("declared_mime", opts.declaredMime);

    xhr.open("POST", `${BASE_URL}/runs/uploads`, true);
    xhr.responseType = "json";

    xhr.upload.onprogress = (evt) => {
      if (opts.onProgress && evt.lengthComputable) {
        opts.onProgress(evt.loaded, evt.total);
      }
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.response as StagedUpload);
      } else {
        const body = xhr.response;
        const message =
          body && typeof body === "object" && "error" in body
            ? (body as { error: string }).error
            : `HTTP ${xhr.status}`;
        reject(new Error(message));
      }
    };
    xhr.onerror = () => reject(new Error("network error"));
    xhr.onabort = () => reject(new DOMException("aborted", "AbortError"));

    if (opts.signal) {
      if (opts.signal.aborted) {
        xhr.abort();
        return;
      }
      opts.signal.addEventListener("abort", () => xhr.abort(), { once: true });
    }

    xhr.send(fd);
  });
}
