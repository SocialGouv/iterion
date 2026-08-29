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
import type {
  AssistantPageContextSnapshot,
  PageContextValue,
} from "./pageContext";
import type { AssistantAuthoringSnapshot } from "@/api/assistantAuthoring";

export const CONTEXT_PREFIX = "[page context:";
export const VISIBLE_PAGE_PREFIX = "<visible-page-context>";
export const VISIBLE_PAGE_SUFFIX = "</visible-page-context>";
export const ACTIVE_EDITOR_PREFIX = "<active-editor-document>";
export const ACTIVE_EDITOR_SUFFIX = "</active-editor-document>";

export interface ActiveEditorDocumentSnapshot {
  sessionId: string;
  revision: number;
  file: string | null;
  complete: boolean;
  sourceLength: number;
  source?: string;
  authoring?: AssistantAuthoringSnapshot;
}

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
  page: AssistantPageContextSnapshot | null = null,
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
  if (page) {
    lines.push(
      `${VISIBLE_PAGE_PREFIX}${serializeVisiblePage(page)}${VISIBLE_PAGE_SUFFIX}`,
    );
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

/**
 * Attach the exact in-memory editor source captured for this send. It is a
 * separate, explicit protocol line rather than part of visible-page-context:
 * page metadata is aggressively bounded/sanitised, while this payload is the
 * operator-authorised document the assistant is meant to reason about.
 */
export function withActiveEditorDocument(
  text: string,
  snapshot: ActiveEditorDocumentSnapshot | null,
): string {
  if (!snapshot || text.trim() === "") return text;
  const json = JSON.stringify(snapshot)
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
  const line = `${ACTIVE_EDITOR_PREFIX}${json}${ACTIVE_EDITOR_SUFFIX}`;
  const contextEnd = text.indexOf("\n\n");
  const hasLeadingPageProtocol =
    text.startsWith(CONTEXT_PREFIX) ||
    text.startsWith(VISIBLE_PAGE_PREFIX) ||
    text.startsWith(ATTACHED_PREFIX);
  if (!hasLeadingPageProtocol || contextEnd < 0) return `${line}\n${text}`;
  // withPageContext owns the leading pointer lines. Keep those first, then
  // place the live document beside them before the operator's prose.
  return `${text.slice(0, contextEnd)}\n${line}${text.slice(contextEnd)}`;
}

/**
 * withoutPageContext strips the machine-generated context lines for DISPLAY.
 *
 * They are protocol, not speech. The operator already sees what the assistant
 * was told — that is what the context chip above the composer is for — so
 * echoing `[page context: view/editor]` inside their own message bubble shows
 * them the same fact twice, in the one place where it reads as something they
 * typed.
 *
 * Display only: what is SENT keeps the lines. Both prefixes are stripped, and
 * only at the top, because that is the only place withPageContext puts them —
 * a bracketed line further down is content, and content is never rewritten.
 */
export function withoutPageContext(text: string): string {
  const lines = text.split("\n");
  let start = 0;
  while (start < lines.length) {
    const line = (lines[start] ?? "").trim();
    if (
      line.startsWith(CONTEXT_PREFIX) ||
      line.startsWith(VISIBLE_PAGE_PREFIX) ||
      line.startsWith(ACTIVE_EDITOR_PREFIX) ||
      line.startsWith(ATTACHED_PREFIX)
    ) {
      start += 1;
      continue;
    }
    break;
  }
  return lines.slice(start).join("\n").trimStart();
}

// Page contributions are deliberately bounded metadata, not a DOM dump. The
// limits keep an editor selection from swallowing the conversation, and the
// key filter is defence in depth for a future view accidentally handing the
// registry credentials. Views must still avoid registering secret values.
const MAX_VISIBLE_PAGE_JSON = 8_000;
const MAX_CONTEXT_STRING = 1_200;
const MAX_CONTEXT_KEYS = 32;
const MAX_CONTEXT_ARRAY = 16;
const MAX_CONTEXT_DEPTH = 6;
const SENSITIVE_KEY =
  /(?:^|[_-])(?:password|passwd|secret|secrets|credential|credentials|authorization|cookie|api[_-]?key|private[_-]?key|access[_-]?token|refresh[_-]?token|env|environment)(?:$|[_-])/i;

export function serializeVisiblePage(
  page: AssistantPageContextSnapshot,
): string {
  const sanitized = sanitizeContextValue(page, 0, new WeakSet<object>());
  let json = JSON.stringify(sanitized);
  if (json.length > MAX_VISIBLE_PAGE_JSON) {
    const compact: AssistantPageContextSnapshot = {
      route: page.route,
      ...(page.title ? { title: page.title } : {}),
      ...(page.section ? { section: page.section } : {}),
      ...(page.entity ? { entity: page.entity } : {}),
      state: { truncated: true },
    };
    json = JSON.stringify(
      sanitizeContextValue(compact, 0, new WeakSet<object>()),
    );
  }
  // JSON escapes quotes and real newlines, but not the XML-like delimiter.
  // Escape angle brackets and the remaining Unicode line separators so page
  // data cannot close the machine-generated line or create a second one.
  return json
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

function sanitizeContextValue(
  value: unknown,
  depth: number,
  seen: WeakSet<object>,
): PageContextValue {
  if (value === null) return null;
  if (typeof value === "string") {
    // C0/C1 controls have no useful place in page metadata. Replace rather
    // than concatenate so two attacker-controlled words cannot be joined into
    // a different identifier by stripping.
    return value
      // eslint-disable-next-line no-control-regex
      .replace(/[\u0000-\u001f\u007f-\u009f]/g, " ")
      .slice(0, MAX_CONTEXT_STRING);
  }
  if (typeof value === "boolean") return value;
  if (typeof value === "number") return Number.isFinite(value) ? value : null;
  if (depth >= MAX_CONTEXT_DEPTH || typeof value !== "object") return null;
  if (seen.has(value)) return null;
  seen.add(value);

  if (Array.isArray(value)) {
    return value
      .slice(0, MAX_CONTEXT_ARRAY)
      .map((item) => sanitizeContextValue(item, depth + 1, seen));
  }

  const out: Record<string, PageContextValue> = {};
  for (const [key, child] of Object.entries(value).slice(0, MAX_CONTEXT_KEYS)) {
    if (SENSITIVE_KEY.test(key)) continue;
    const safeKey = key
      // eslint-disable-next-line no-control-regex
      .replace(/[\u0000-\u001f\u007f-\u009f]/g, "")
      .slice(0, 80);
    if (!safeKey) continue;
    out[safeKey] = sanitizeContextValue(child, depth + 1, seen);
  }
  return out;
}
