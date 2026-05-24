// Findings inbox — client for /api/v1/findings.
//
// Findings are short markdown notes a bot run leaves in
// ${PROJECT_MEMORY_DIR}/findings/ when it spots something the
// operator might want to act on but that doesn't fit the current
// roadmap. The server-side surface parses YAML-ish frontmatter
// (see pkg/server/findings.go) and exposes the list + a DELETE for
// archiving. Both endpoints share the studio's apiRequest base.

import { apiRequest } from "./client";

const BASE = "/api/v1/findings";

export interface Finding {
  filename: string;
  path: string;
  size_bytes: number;
  modified_at: string; // RFC3339
  title?: string;
  description?: string;
  kind?: string;
  source_bot?: string;
  tags?: string[];
  body?: string;
  body_truncated?: boolean;
}

export function listFindings(): Promise<Finding[]> {
  return apiRequest<Finding[]>(BASE);
}

export function deleteFinding(filename: string): Promise<{ deleted: string }> {
  return apiRequest<{ deleted: string }>(
    `${BASE}/${encodeURIComponent(filename)}`,
    { method: "DELETE" },
  );
}
