import type { IterDocument } from "./types";

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
): Promise<{ diagnostics: string[]; warnings: string[] }> {
  return request("/validate", {
    method: "POST",
    body: JSON.stringify({ document }),
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
