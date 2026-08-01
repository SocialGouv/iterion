// How a reference actually reaches the assistant.
//
// A single leading line carrying the POINTER — never the page's content.
// The assistant resolves it with the tools it already has, so a huge run
// costs this message one line, and the operator sees the same string in
// the transcript that the pinned chip shows. Invisible context is the
// failure mode this whole feature exists to avoid.
//
// The consumer side of this protocol is the bot's own contract — for
// Nexie, the "Page context from the studio" section of `prompt
// nexie_system:` in bots/whats-next/main.bot, which maps each kind to a
// tool it actually holds. Changing CONTEXT_PREFIX or the reference
// vocabulary means changing that section too.

import { sanitizeReferenceText } from "./routeReference";
import type { TypedReference } from "./routeReference";

export const CONTEXT_PREFIX = "[page context:";

/**
 * withPageContext prefixes an outbound message with the reference the
 * operator can see pinned above the composer. Returns the text unchanged
 * when there is no active reference (no route match, or dismissed).
 */
export function withPageContext(
  text: string,
  reference: TypedReference | null,
): string {
  const trimmed = text.trim();
  if (!reference || trimmed === "") return trimmed;
  // This function owns the delimiter, so it enforces it — even though
  // routeReference mints every reference sanitised already. A reference
  // reaching here from somewhere else (an explicit drop chip, a future
  // caller) must not be able to break the bracket or the line and land
  // attacker-authored text at the top of the operator's own message.
  return `${CONTEXT_PREFIX} ${sanitizeReferenceText(reference.ref)}]\n\n${trimmed}`;
}
