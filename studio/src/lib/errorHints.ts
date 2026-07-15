// errorHints turns a raw thrown value (an Error, a string, an ApiError,
// or a stringified Go-side failure) into a friendly, actionable line.
// It is the generalisation of TrackerErrorBanner's substring-matching
// guidance: instead of dumping `HTTP 404: not found` or
// `fork/exec …: no such file or directory` at the user, surfaces map
// common failure modes to a short title + what-to-do hint.
//
// One source of truth, two renderers:
//   - toasts  → toastError(addToast, err, context)
//   - inline  → <ErrorNotice error={err} context="…" /> (shared component)

export interface ErrorHint {
  /** Short, human title that replaces the raw error as the headline. */
  title: string;
  /** Optional second line: what the user can do about it. */
  hint?: string;
}

interface ErrorHintRule {
  // Tested (case-insensitively) against the normalised message. First
  // match wins, so order narrower patterns before broader ones.
  match: RegExp;
  title: string;
  hint?: string;
}

// Ordered most-specific → least-specific. Notably, the filesystem and
// "tool not installed" / "command not found" rules come before the
// generic `not found` (HTTP 404) rule, since their messages contain the
// substring "not found".
const RULES: ErrorHintRule[] = [
  {
    match: /\beacces\b|permission denied/i,
    title: "Permission denied",
    hint: "iterion can't read or write this path. Check the folder's permissions, or choose another location.",
  },
  {
    // Must precede ENOENT: a `fork/exec …: no such file or directory`
    // is a missing executable, not a missing user file.
    match: /command not found|executable file not found|fork\/exec.*no such file/i,
    title: "Tool not installed",
    hint: "An external CLI this run needs isn't installed or on PATH.",
  },
  {
    match: /\benoent\b|no such file or directory/i,
    title: "File not found",
    hint: "The file was moved, renamed, or deleted. Refresh and try again.",
  },
  {
    match: /\beexist\b|file (already )?exists/i,
    title: "Already exists",
    hint: "Something with this name already exists. Choose a different name.",
  },
  {
    match: /\benotdir\b|not a directory/i,
    title: "Not a folder",
    hint: "That path points to a file, not a folder. Pick the parent directory.",
  },
  {
    match: /\beconnrefused\b|connection refused/i,
    title: "Can't reach the server",
    hint: "The iterion server isn't reachable on this port. Make sure it's running.",
  },
  {
    match:
      /no such host|dial tcp|i\/o timeout|\betimedout\b|failed to fetch|networkerror|network ?error/i,
    title: "Network unreachable",
    hint: "Check your connection and the configured base URL, then retry.",
  },
  {
    // Git revision-range failures (run-commits/diff endpoints): the
    // storage branch or base commit isn't reachable in this checkout —
    // typical after a worktree GC, or when inspecting a run from
    // another clone. Must precede the HTTP-status rules: the backend
    // wraps the git stderr in an "API error 500" envelope, which would
    // otherwise match the generic 5xx rule.
    match: /invalid revision range|unknown revision|bad revision|not a valid ref/i,
    title: "Run branch unavailable",
    hint: "The run branch isn't available in this repo — it may have been pruned or live in another checkout.",
  },
  {
    match: /\b401\b|unauthorized|bad credentials|token_invalidated/i,
    title: "Authentication rejected",
    hint: "The token is missing, expired, or lacks scope. Regenerate it in Settings.",
  },
  {
    match: /\b403\b|forbidden|rate limit/i,
    title: "Refused or rate-limited",
    hint: "The service is refusing or throttling the request. Wait a moment and retry.",
  },
  {
    match: /\b404\b|not found/i,
    title: "Not found",
    hint: "The resource no longer exists on the server. Refresh the view.",
  },
  {
    match: /\b5\d\d\b|internal server error/i,
    title: "Server error",
    hint: "iterion's backend returned an error. Check the server logs, then retry.",
  },
  {
    match: /binary file/i,
    title: "Binary file",
    hint: "iterion can't edit binary files inline. Open it in an external editor.",
  },
  {
    match: /\baborted\b|context canceled|signal: killed/i,
    title: "Cancelled",
    hint: "The operation was cancelled.",
  },
  {
    match: /unmarshal|invalid character|unexpected end of json|parse error/i,
    title: "Unexpected response",
    hint: "The server returned data iterion couldn't read. Refresh; if it persists, file a bug.",
  },
];

// Normalise any thrown value to a readable string. Replaces the
// scattered `err instanceof Error ? err.message : String(err)` idiom
// (which prints "[object Object]" for ApiError-like throwables).
export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === "string") return err;
  if (err && typeof err === "object") {
    const m = (err as { message?: unknown }).message;
    if (typeof m === "string" && m.length > 0) return m;
    try {
      return JSON.stringify(err);
    } catch {
      return String(err);
    }
  }
  return String(err);
}

// Returns a friendly hint for a recognised failure mode, or null when
// nothing matches (the caller then falls back to the raw message).
export function errorHint(err: unknown): ErrorHint | null {
  const msg = errorMessage(err);
  for (const rule of RULES) {
    if (rule.match.test(msg)) {
      return rule.hint ? { title: rule.title, hint: rule.hint } : { title: rule.title };
    }
  }
  return null;
}

type AddToast = (
  message: string,
  type: "success" | "error" | "info" | "warning",
  opts?: {
    action?: { label: string; onClick: () => void };
    persistent?: boolean;
  },
) => void;

// Emit an error toast with a friendly headline. `context` names the
// operation ("Open file failed", "Install failed") and is prepended to
// the inferred title (or the raw message when nothing matches).
export function toastError(
  addToast: AddToast,
  err: unknown,
  context?: string,
  opts?: { action?: { label: string; onClick: () => void }; persistent?: boolean },
): void {
  const hint = errorHint(err);
  const base = hint?.title ?? errorMessage(err);
  const message = context ? `${context}: ${base}` : base;
  addToast(message, "error", opts);
}
