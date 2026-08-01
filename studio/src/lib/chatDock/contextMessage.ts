// How a reference actually reaches the assistant.
//
// A single leading line carrying the POINTER — never the page's content.
// The assistant resolves it with the tools it already has (__mcp-control
// for a run, __mcp-board for a card), so a huge run costs this message
// one line, and the operator sees the same string in the transcript that
// the pinned chip shows. Invisible context is the failure mode this
// whole feature exists to avoid.

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
  return `${CONTEXT_PREFIX} ${reference.ref}]\n\n${trimmed}`;
}
