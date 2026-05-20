// Bot registry — REST client. Mirrors pkg/server/bots_routes.go.
// All paths are relative to the studio's same-origin server.

import { apiRequest } from "./client";
import type { PresetsBlock, VarsBlock } from "./types";

const BASE = "/api/v1/bots";

// ---------------------------------------------------------------------------
// Types — mirror pkg/botregistry JSON tags
// ---------------------------------------------------------------------------

/** BotEntry is the metadata-only shape returned by the registry list
 *  endpoint and embedded inside BotEntryWithSchema. */
export interface BotEntry {
  name: string;
  description?: string;
  path: string;
  triggers?: string[];
  capabilities?: string[];
}

/** BotEntryWithSchema augments BotEntry with the workflow's declared
 *  vars + presets — same JSON shape as the studio's existing
 *  VarsBlock/PresetsBlock so VarFieldInput consumes it unchanged. */
export interface BotEntryWithSchema extends BotEntry {
  vars?: VarsBlock;
  presets?: PresetsBlock;
  /** Non-empty when the bot's source failed to parse. The picker still
   *  shows the bot but the typed form is hidden / surfaces an error. */
  schema_error?: string;
}

interface ListResponse {
  bots: BotEntryWithSchema[];
}

// ---------------------------------------------------------------------------
// REST surface
// ---------------------------------------------------------------------------

/** listBots returns every bot the host knows about along with its
 *  vars/presets schema. The full schemas are bundled in the list
 *  payload so the picker can switch bots without a second round trip. */
export async function listBots(): Promise<BotEntryWithSchema[]> {
  const r = await apiRequest<ListResponse>(BASE);
  return r.bots ?? [];
}

/** getBot fetches a single bot by name with its full schema. Useful
 *  when a ticket references a bot the list endpoint hasn't loaded
 *  yet (cache miss / page reload while a modal is open). */
export function getBot(name: string): Promise<BotEntryWithSchema> {
  return apiRequest<BotEntryWithSchema>(`${BASE}/${encodeURIComponent(name)}`);
}
