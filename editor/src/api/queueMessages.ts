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

export async function queueMessage(
  runId: string,
  text: string,
): Promise<QueuedUserMessage> {
  return request<QueuedUserMessage>(
    `/runs/${encodeURIComponent(runId)}/queue-message`,
    {
      method: "POST",
      body: JSON.stringify({ text }),
    },
  );
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
