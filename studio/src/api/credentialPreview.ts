import type { components } from "./schema";
import { request } from "./client";

export type CredentialPreview = components["schemas"]["CredentialPreview"];
export type CredentialPreviewRequest = components["schemas"]["CredentialPreviewRequest"];
export type CredentialPreviewCandidate = components["schemas"]["CredentialPreviewCandidate"];

export function previewCredentials(teamID: string, input: CredentialPreviewRequest): Promise<CredentialPreview> {
  return request(`/teams/${encodeURIComponent(teamID)}/credentials/preview`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}
