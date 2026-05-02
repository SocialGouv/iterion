/**
 * Low-level child_process wrapper around the iterion CLI.
 *
 * Uses `spawn` (not `exec`) to avoid stdout buffer overflows and to
 * support line-buffered streaming of stderr (where iterion writes its
 * structured logs — see `iterlog.New(level, os.Stderr)` in
 * `pkg/cli/run.go`).
 */

import { spawn } from "node:child_process";
import { Readable } from "node:stream";
import { createInterface } from "node:readline";

import { IterionInvocationError, parseRuntimeError } from "./errors.js";

export interface ExecOptions {
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  signal?: AbortSignal;
  timeoutMs?: number;
  /** Optional stdin payload. */
  stdin?: string;
  /** Called for each complete line written to the child's stderr. */
  onStderrLine?: (line: string) => void;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number | null;
  signal: NodeJS.Signals | null;
}

/** Spawn the iterion binary and capture its output. */
export async function execIterion(
  bin: string,
  args: readonly string[],
  opts: ExecOptions = {},
): Promise<ExecResult> {
  return new Promise<ExecResult>((resolvePromise, rejectPromise) => {
    const child = spawn(bin, args as string[], {
      cwd: opts.cwd,
      env: opts.env ?? process.env,
      stdio: ["pipe", "pipe", "pipe"],
    });

    let stdout = "";
    let stderr = "";
    let timedOut = false;
    let aborted = false;
    let settled = false;

    const stderrLines = opts.onStderrLine
      ? createInterface({ input: child.stderr as Readable, crlfDelay: Infinity })
      : null;

    if (stderrLines && opts.onStderrLine) {
      stderrLines.on("line", opts.onStderrLine);
    }

    child.stdout.on("data", (chunk: Buffer | string) => {
      stdout += chunk.toString();
    });
    child.stderr.on("data", (chunk: Buffer | string) => {
      stderr += chunk.toString();
    });

    const timer =
      opts.timeoutMs && opts.timeoutMs > 0
        ? setTimeout(() => {
            timedOut = true;
            child.kill("SIGTERM");
            // Hard kill if SIGTERM is ignored.
            setTimeout(() => child.kill("SIGKILL"), 2000).unref();
          }, opts.timeoutMs)
        : null;

    const onAbort = () => {
      aborted = true;
      child.kill("SIGTERM");
    };

    if (opts.signal) {
      if (opts.signal.aborted) {
        onAbort();
      } else {
        opts.signal.addEventListener("abort", onAbort, { once: true });
      }
    }

    if (opts.stdin !== undefined) {
      child.stdin.end(opts.stdin);
    } else {
      child.stdin.end();
    }

    child.on("error", (err) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (opts.signal) opts.signal.removeEventListener("abort", onAbort);
      rejectPromise(
        new IterionInvocationError(
          `failed to spawn iterion: ${(err as Error).message}`,
          bin,
          args,
          null,
          stdout,
          stderr,
          err,
        ),
      );
    });

    child.on("close", (code, signal) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (opts.signal) opts.signal.removeEventListener("abort", onAbort);
      stderrLines?.close();

      if (timedOut) {
        rejectPromise(
          new IterionInvocationError(
            `iterion timed out after ${opts.timeoutMs}ms`,
            bin,
            args,
            code,
            stdout,
            stderr,
          ),
        );
        return;
      }
      if (aborted) {
        rejectPromise(
          new IterionInvocationError(
            `iterion was aborted by caller`,
            bin,
            args,
            code,
            stdout,
            stderr,
          ),
        );
        return;
      }
      resolvePromise({ stdout, stderr, exitCode: code, signal });
    });
  });
}

/**
 * Strict JSON parser; on failure throws an `IterionInvocationError` so
 * callers see the offending stdout/stderr.
 */
export function parseJSON<T>(
  stdout: string,
  ctx: { bin: string; args: readonly string[]; stderr: string; exitCode: number | null },
): T {
  const trimmed = stdout.trim();
  if (!trimmed) {
    throw new IterionInvocationError(
      "iterion produced no output to parse as JSON",
      ctx.bin,
      ctx.args,
      ctx.exitCode,
      stdout,
      ctx.stderr,
    );
  }
  try {
    return JSON.parse(trimmed) as T;
  } catch (err) {
    throw new IterionInvocationError(
      `iterion output is not valid JSON: ${(err as Error).message}`,
      ctx.bin,
      ctx.args,
      ctx.exitCode,
      stdout,
      ctx.stderr,
      err,
    );
  }
}

/**
 * Lenient JSON parser that returns the FIRST top-level JSON value present
 * in `stdout`. Tolerates trailing content (extra JSON objects, log noise)
 * after the first value.
 *
 * Used by paths where the CLI may emit two concatenated JSON envelopes —
 * for example `iterion --json validate <bad-file>` writes the structured
 * `{file, valid:false, parse_diagnostics:[…]}` envelope and is then
 * followed by the outer error handler's `{"error":"validation failed"}`
 * envelope (cmd/iterion/main.go). The structured payload is what the SDK
 * needs to expose.
 */
export function parseFirstJSON<T>(
  stdout: string,
  ctx: { bin: string; args: readonly string[]; stderr: string; exitCode: number | null },
): T {
  const trimmed = stdout.trim();
  if (!trimmed) {
    throw new IterionInvocationError(
      "iterion produced no output to parse as JSON",
      ctx.bin,
      ctx.args,
      ctx.exitCode,
      stdout,
      ctx.stderr,
    );
  }

  // Fast path: the whole stdout is a single JSON value.
  try {
    return JSON.parse(trimmed) as T;
  } catch {
    // Fall through to incremental parse.
  }

  // Walk the buffer to find the end of the first complete top-level JSON
  // value. Handles strings (with escapes) so that `}` inside a string
  // literal does not falsely close an object.
  const end = findFirstJSONValueEnd(trimmed);
  if (end > 0) {
    const head = trimmed.slice(0, end);
    try {
      return JSON.parse(head) as T;
    } catch (err) {
      throw new IterionInvocationError(
        `iterion output is not valid JSON: ${(err as Error).message}`,
        ctx.bin,
        ctx.args,
        ctx.exitCode,
        stdout,
        ctx.stderr,
        err,
      );
    }
  }

  throw new IterionInvocationError(
    `iterion output is not valid JSON`,
    ctx.bin,
    ctx.args,
    ctx.exitCode,
    stdout,
    ctx.stderr,
  );
}

// findFirstJSONValueEnd returns the exclusive end index of the first
// complete top-level JSON value in `s`, or 0 if none was found. Only
// supports objects, arrays, and primitives at the top level; assumes
// UTF-16 string semantics consistent with JS source.
function findFirstJSONValueEnd(s: string): number {
  const first = s[0];
  if (first === "{" || first === "[") {
    return findBracketedEnd(s, first);
  }
  // Primitive top-level value — let JSON.parse find the first newline-
  // terminated token. Iterion never emits primitive top-level JSON, so
  // this branch is here only for completeness.
  const nl = s.indexOf("\n");
  return nl > 0 ? nl : s.length;
}

function findBracketedEnd(s: string, open: "{" | "["): number {
  const close = open === "{" ? "}" : "]";
  let depth = 0;
  let inString = false;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (inString) {
      if (ch === "\\") {
        i++; // skip escaped char
        continue;
      }
      if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"') {
      inString = true;
      continue;
    }
    if (ch === open) depth++;
    else if (ch === close) {
      depth--;
      if (depth === 0) return i + 1;
    }
  }
  return 0;
}

/**
 * Build the standard "command failed" invocation error, attaching a
 * parsed `IterionRuntimeError` as `cause` when the stderr matches the
 * `error [CODE]:` preamble.
 */
export function buildInvocationError(
  message: string,
  bin: string,
  args: readonly string[],
  result: ExecResult,
): IterionInvocationError {
  const runtimeErr = parseRuntimeError(result.stderr);
  return new IterionInvocationError(
    message,
    bin,
    args,
    result.exitCode,
    result.stdout,
    result.stderr,
    runtimeErr ?? undefined,
  );
}
