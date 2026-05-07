// Mirrors pkg/backend/detect.Report. Keep the field names in sync — the
// Go handler returns json:"snake_case" and we deserialise verbatim.

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

export interface BackendStatus {
  name: "claude_code" | "codex" | "claw";
  available: boolean;
  auth: "oauth" | "api_key" | "none";
  sources: string[];
  hints?: string[];
}

export interface ProviderStatus {
  name: "anthropic" | "openai" | "foundry" | "bedrock" | "vertex";
  available: boolean;
  source: string;
  suggested_model?: string;
}

export interface BackendDetectReport {
  preference_order: string[];
  resolved_default: string;
  backends: BackendStatus[];
  providers: ProviderStatus[];
}

export async function fetchBackendDetect(
  signal?: AbortSignal,
): Promise<BackendDetectReport> {
  const res = await fetch(`${BASE_URL}/backends/detect`, {
    credentials: "include",
    signal,
  });
  if (!res.ok) {
    throw new Error(`backends/detect: HTTP ${res.status}`);
  }
  return (await res.json()) as BackendDetectReport;
}
