import type { IterDocument, FileEntry, ListFilesResponse, SaveFileResponse } from "./types";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    throw new Error(`API error ${res.status}: ${await res.text()}`);
  }
  return res.json() as Promise<T>;
}

export async function parseSource(
  source: string,
): Promise<{ document: IterDocument; diagnostics: string[] }> {
  return request("/parse", {
    method: "POST",
    body: JSON.stringify({ source }),
  });
}

export async function unparse(document: IterDocument): Promise<string> {
  const res = await request<{ source: string }>("/unparse", {
    method: "POST",
    body: JSON.stringify({ document }),
  });
  return res.source;
}

export async function validate(
  document: IterDocument,
  signal?: AbortSignal,
): Promise<{ diagnostics: string[]; warnings: string[] }> {
  return request("/validate", {
    method: "POST",
    body: JSON.stringify({ document }),
    signal,
  });
}

export async function listExamples(): Promise<string[]> {
  return request("/examples");
}

export async function loadExample(
  name: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[] }> {
  return request(`/examples/${encodeURIComponent(name)}`);
}

// File management

export async function listFiles(): Promise<FileEntry[]> {
  const res = await request<ListFilesResponse>("/files");
  return res.files;
}

export async function openFile(
  path: string,
): Promise<{ source: string; document: IterDocument; diagnostics: string[]; path: string }> {
  return request("/files/open", {
    method: "POST",
    body: JSON.stringify({ path }),
  });
}

export async function saveFile(
  path: string,
  document: IterDocument,
): Promise<SaveFileResponse> {
  return request("/files/save", {
    method: "POST",
    body: JSON.stringify({ path, document }),
  });
}
