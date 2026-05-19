// Operator chat-message inbox REST client. Mirrors the
// POST/DELETE/GET surface registered in pkg/server/runs.go.

import { request } from "@/api/client";
import type { QueuedUserMessage } from "@/store/run";

export async function listQueuedMessages(
  runId: string,
): Promise<QueuedUserMessage[]> {
  const out = await request<{ messages: QueuedUserMessage[] }>(
    `/runs/${encodeURIComponent(runId)}/queue-messages`,
  );
  return out.messages ?? [];
}

export interface QueueMessageOptions {
  // skills, when non-empty, is the list of bundle skill names to
  // attach to the queued message. The engine mirrors each referenced
  // SKILL.md into the run's .claude/skills/ before injecting the
  // message into the agent's conversation. Sticky for the rest of
  // the run.
  skills?: string[];
}

export async function queueMessage(
  runId: string,
  text: string,
  opts: QueueMessageOptions = {},
): Promise<QueuedUserMessage> {
  return request<QueuedUserMessage>(
    `/runs/${encodeURIComponent(runId)}/queue-message`,
    {
      method: "POST",
      body: JSON.stringify({ text, skills: opts.skills && opts.skills.length > 0 ? opts.skills : undefined }),
    },
  );
}

// BundleSkill mirrors the runview.BundleSkill JSON wire shape.
export interface BundleSkill {
  name: string;
  description?: string;
  path: string;
}

// listRunSkills fetches the bundle skill catalog for a run. Returns
// an empty array when the run has no backing bundle, when the bundle
// has no skills/ directory, or when the bundle is a .botz archive
// (catalogue is only supported on directory-form bundles in Phase 4).
export async function listRunSkills(runId: string): Promise<BundleSkill[]> {
  return request<BundleSkill[]>(`/runs/${encodeURIComponent(runId)}/skills`);
}

export async function cancelQueuedMessage(
  runId: string,
  msgId: string,
): Promise<void> {
  await request<void>(
    `/runs/${encodeURIComponent(runId)}/queue-message/${encodeURIComponent(msgId)}`,
    { method: "DELETE" },
  );
}
