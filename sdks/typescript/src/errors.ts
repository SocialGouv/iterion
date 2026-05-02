/**
 * Typed errors surfaced by the iterion SDK.
 *
 * The CLI prints structured runtime errors to stderr in the form:
 *
 *   error [CODE]: message
 *     node: <node_id>
 *     hint: <hint>
 *
 * (See `cli.PrintError` in `pkg/cli/output.go`.) We parse that back out
 * into `IterionRuntimeError` so callers can branch on `code` rather than
 * matching strings.
 */

import type { RuntimeErrorCode } from "./types.js";

/** Base class for every error thrown by the SDK. */
export class IterionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "IterionError";
    // Restore prototype chain for instanceof across compiled output.
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/** Raised when the iterion binary cannot be located on the host. */
export class IterionBinaryNotFoundError extends IterionError {
  constructor(
    public readonly searchedPaths: string[],
    public readonly hint = "Install iterion from https://github.com/SocialGouv/iterion or set the ITERION_BIN environment variable.",
  ) {
    super(
      `iterion binary not found. Searched: ${searchedPaths.join(", ") || "(none)"}. ${hint}`,
    );
    this.name = "IterionBinaryNotFoundError";
  }
}

/**
 * Raised when invoking the iterion CLI fails outside of a typed runtime
 * error (non-zero exit with no parseable error code, JSON parse failure,
 * unexpected output shape).
 */
export class IterionInvocationError extends IterionError {
  constructor(
    message: string,
    public readonly cmd: string,
    public readonly args: readonly string[],
    public readonly exitCode: number | null,
    public readonly stdout: string,
    public readonly stderr: string,
    public readonly cause?: unknown,
  ) {
    super(message);
    this.name = "IterionInvocationError";
  }
}

/** Structured equivalent of `runtime.RuntimeError` from the Go engine. */
export class IterionRuntimeError extends IterionError {
  constructor(
    public readonly code: RuntimeErrorCode,
    message: string,
    public readonly nodeId?: string,
    public readonly hint?: string,
    public readonly stderr?: string,
  ) {
    super(message);
    this.name = "IterionRuntimeError";
  }
}

/**
 * Raised by `IterionClient.run` / `resume` only when the caller opts in
 * via `{ throwOn: [...] }`. Not raised by default — the standard contract
 * is to return the result so the consumer can branch on `status`.
 */
export class IterionRunPausedSignal extends IterionError {
  constructor(
    public readonly runId: string,
    public readonly interactionId?: string,
    public readonly nodeId?: string,
    public readonly questions?: Record<string, unknown>,
  ) {
    super(`run ${runId} paused waiting for human input`);
    this.name = "IterionRunPausedSignal";
  }
}

const RUNTIME_ERROR_LINE = /^error\s*\[([A-Z0-9_]+)\]\s*:\s*(.+)$/m;
const NODE_LINE = /^\s*node:\s*(.+)$/m;
const HINT_LINE = /^\s*hint:\s*(.+)$/m;

/**
 * Parse a CLI stderr blob into a structured `IterionRuntimeError`.
 *
 * Returns `null` if the stderr does not contain the `error [CODE]: ...`
 * preamble that `cli.PrintError` emits for `runtime.RuntimeError`s.
 */
export function parseRuntimeError(stderr: string): IterionRuntimeError | null {
  if (!stderr) {
    return null;
  }
  const match = RUNTIME_ERROR_LINE.exec(stderr);
  if (!match) {
    return null;
  }
  const code = match[1] as RuntimeErrorCode;
  const message = match[2]!.trim();
  const node = NODE_LINE.exec(stderr)?.[1]?.trim();
  const hint = HINT_LINE.exec(stderr)?.[1]?.trim();
  return new IterionRuntimeError(code, message, node, hint, stderr);
}
