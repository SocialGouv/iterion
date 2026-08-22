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

// The EXPLICIT half (#333). A separate prefix rather than more entries on the
// page-context line, because the two mean different things to the bot: the
// page reference disambiguates the operator's words, an attached one is the
// thing they are asking ABOUT. Collapsing them would lose exactly the
// distinction that makes a drop worth making.
export const ATTACHED_PREFIX = "[attached:";

// Same reasoning as the composed-length cap in routeReference: the header is
// a pointer list, and a message whose first lines scroll is one where the
// operator can no longer see what they attached.
const MAX_ATTACHED_ON_WIRE = 8;

/**
 * withPageContext prefixes an outbound message with the references the
 * operator can see above their composer: the pinned page one (implicit) and
 * anything they dropped in (explicit). Returns the text unchanged when there
 * is neither (no route match, dismissed, nothing attached).
 */
export function withPageContext(
  text: string,
  reference: TypedReference | null,
  attached: readonly TypedReference[] = [],
): string {
  const trimmed = text.trim();
  if (trimmed === "") return trimmed;

  const lines: string[] = [];
  // This function owns the delimiters, so it enforces them — even though
  // routeReference mints every reference sanitised already. A reference
  // reaching here from somewhere else (an explicit drop chip, a future
  // caller) must not be able to break the bracket or the line and land
  // attacker-authored text at the top of the operator's own message.
  if (reference) {
    lines.push(`${CONTEXT_PREFIX} ${sanitizeReferenceText(reference.ref)}]`);
  }
  if (attached.length > 0) {
    const refs = attached
      .slice(0, MAX_ATTACHED_ON_WIRE)
      .map((r) => sanitizeReferenceText(r.ref))
      // A ref that sanitises to nothing would leave a bare comma, which is
      // a malformed pointer the bot is told to distrust.
      .filter((r) => r !== "");
    if (refs.length > 0) {
      lines.push(`${ATTACHED_PREFIX} ${refs.join(", ")}]`);
    }
  }
  if (lines.length === 0) return trimmed;
  return `${lines.join("\n")}\n\n${trimmed}`;
}
