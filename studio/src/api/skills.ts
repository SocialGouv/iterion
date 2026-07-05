// Local (non-cloud) skill-library REST client. Mirrors
// pkg/server/local_skills_routes.go. Single-operator, unauthenticated,
// available only in local mode (server_info.skills_enabled). Skills are keyed
// by name (a single path segment) — the DSL `skills:` reference and the on-disk
// directory. No sealing: a skill is public guidance text.

import { request } from "@/api/client";
import { guard404 } from "@/api/client";

export interface LibrarySkill {
  name: string;
  description?: string;
  scope: "global" | "project";
  body?: string;
}

export interface UpsertLocalSkillInput {
  name: string;
  body: string;
  scope?: "global" | "project";
}

export async function listLocalSkills(): Promise<LibrarySkill[]> {
  return guard404("skills", async () => {
    const r = await request<{ skills: LibrarySkill[] }>(`/local/skills`);
    return r.skills ?? [];
  });
}

export function getLocalSkill(name: string): Promise<LibrarySkill> {
  return guard404("skills", () =>
    request<LibrarySkill>(`/local/skills/${encodeURIComponent(name)}`),
  );
}

// createLocalSkill creates or overwrites a skill (POST). The server upserts by
// name at the requested scope.
export function createLocalSkill(input: UpsertLocalSkillInput): Promise<LibrarySkill> {
  return guard404("skills", () =>
    request<LibrarySkill>(`/local/skills`, {
      method: "POST",
      body: JSON.stringify(input),
    }),
  );
}

// updateLocalSkill overwrites an existing skill's body (PUT {name}).
export function updateLocalSkill(
  name: string,
  input: Omit<UpsertLocalSkillInput, "name">,
): Promise<LibrarySkill> {
  return guard404("skills", () =>
    request<LibrarySkill>(`/local/skills/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: JSON.stringify(input),
    }),
  );
}

export async function deleteLocalSkill(
  name: string,
  scope: "global" | "project",
): Promise<void> {
  await guard404("skills", () =>
    request<void>(
      `/local/skills/${encodeURIComponent(name)}?scope=${encodeURIComponent(scope)}`,
      { method: "DELETE" },
    ),
  );
}

// isValidSkillName mirrors skilllib.ValidName: a single path segment of
// letters/digits/`.`/`-`/`_`, not starting with a dot.
export function isValidSkillName(name: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(name);
}
