// Structured ask_user options — mirror of the backend's reserved
// presentation keys (pkg/backend/delegate: AskUserOptionsKey /
// AskUserAllowFreeTextKey). When an agent calls ask_user with an
// `options` list, the pause's questions map carries these keys so the
// UI can render clickable choices instead of a bare textarea. The
// answer wire shape is unchanged: a plain string under
// `ask_user_response` (the picked option's id, or typed free text).

export interface AskUserOption {
  id: string;
  label: string;
}

export const ASK_USER_RESPONSE_KEY = "ask_user_response";
const ASK_USER_OPTIONS_KEY = "_ask_user_options";
const ASK_USER_ALLOW_FREE_TEXT_KEY = "_ask_user_allow_free_text";
export const PERMISSION_MARKER_KEY = "_permission";

/** Host-stamped details for an ask-mode permission pause. */
export interface PermissionMarker {
  tool?: string;
  input?: Record<string, unknown>;
  rule?: string;
}

/**
 * A permission gate is distinct from an ordinary free-text ask_user turn:
 * its answer is an exact authorization token, not operator prose. Keep this
 * shared with the run-console approval form so the two Studio surfaces agree.
 */
export function permissionMarker(
  questions: Record<string, unknown> | null | undefined,
): PermissionMarker | null {
  const marker = questions?.[PERMISSION_MARKER_KEY];
  if (marker && typeof marker === "object" && !Array.isArray(marker)) {
    return marker as PermissionMarker;
  }
  return null;
}

/** Parse the structured options off a pause's questions map ([] when absent/malformed). */
export function askUserOptions(questions: Record<string, unknown> | null | undefined): AskUserOption[] {
  const raw = questions?.[ASK_USER_OPTIONS_KEY];
  if (!Array.isArray(raw)) return [];
  const out: AskUserOption[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const { id, label } = item as { id?: unknown; label?: unknown };
    if (typeof id === "string" && id && typeof label === "string" && label) {
      out.push({ id, label });
    }
  }
  return out;
}

/**
 * Whether the pause accepts typed free text. Without options the answer
 * is always free text (the historical single-textarea shape); with
 * options the backend stamps the explicit flag (absent → false, per the
 * ask_user tool contract).
 */
export function askUserAllowsFreeText(questions: Record<string, unknown> | null | undefined): boolean {
  if (askUserOptions(questions).length === 0) return true;
  return questions?.[ASK_USER_ALLOW_FREE_TEXT_KEY] === true;
}

/**
 * Question-map keys that are runtime plumbing, not operator-facing
 * fields (options payload, permission marker, queued-messages stash…).
 * Forms must not render them as answerable inputs.
 */
export function isReservedQuestionKey(key: string): boolean {
  return key.startsWith("_");
}
