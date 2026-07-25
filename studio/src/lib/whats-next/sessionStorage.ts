// Tiny wrappers around localStorage to remember the current whats-next
// run id per scope. Survives a page reload so reopening /whats-next
// resumes the in-flight conversation instead of presenting a fresh
// launcher.
//
// Keyed by (botId, scope): the scope is the local project dir in
// local/desktop mode and `(team, active repo)` in cloud — switching the
// sidebar repo must switch conversations, not silently resurface a
// session bound to another repo.

import { readStringFlag, writeStringFlag, removeFlag } from "../localStorageFlag";

const KEY_PREFIX = "iterion.whats-next.runId";

function key(botId: string, scope: string | null | undefined): string {
  return `${KEY_PREFIX}.${botId}.${scope ?? "_default"}`;
}

export function rememberSessionRunId(
  botId: string,
  scope: string | null | undefined,
  runId: string,
): void {
  writeStringFlag(key(botId, scope), runId);
}

export function recallSessionRunId(
  botId: string,
  scope: string | null | undefined,
): string | null {
  const raw = readStringFlag(key(botId, scope));
  return raw.length > 0 ? raw : null;
}

export function forgetSessionRunId(
  botId: string,
  scope: string | null | undefined,
): void {
  removeFlag(key(botId, scope));
}
