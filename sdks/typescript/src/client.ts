/**
 * High-level TypeScript façade over the iterion CLI.
 *
 * Each method maps 1:1 to a CLI subcommand. Option names are the
 * camelCase TS equivalent of CLI flags. The SDK always passes `--json`
 * so output can be parsed into typed result objects.
 */

import { partitionAnswers, writeAnswersFile } from "./answers.js";
import { resolveBinary } from "./binary.js";
import {
  IterionInvocationError,
  IterionRunPausedSignal,
  IterionRuntimeError,
  parseRuntimeError,
} from "./errors.js";
import {
  buildInvocationError,
  execIterion,
  parseFirstJSON,
  parseJSON,
  type ExecOptions,
  type ExecResult,
} from "./exec.js";
import { tailEvents, type TailEventsOptions } from "./events.js";
import {
  loadArtifact as storeLoadArtifact,
  loadInteraction as storeLoadInteraction,
  loadRun as storeLoadRun,
  listRuns as storeListRuns,
} from "./store.js";
import type {
  Artifact,
  DiagramResult,
  Event,
  InspectResult,
  Interaction,
  JsonValue,
  ReportResult,
  ResumeResult,
  Run,
  RunResult,
  ValidateResult,
  VersionResult,
} from "./types.js";

// ---------------------------------------------------------------------------
// Construction options
// ---------------------------------------------------------------------------

export interface IterionClientOptions {
  /** Explicit binary path. Overrides `ITERION_BIN` and PATH lookup. */
  binPath?: string;
  /** Working directory for child processes. Default: `process.cwd()`. */
  cwd?: string;
  /** Extra environment variables merged onto `process.env`. */
  env?: Record<string, string | undefined>;
  /** Default `--store-dir`. Applied unless overridden per-call. */
  storeDir?: string;
  /** Default abort signal applied to every invocation. Per-call signals take precedence. */
  signal?: AbortSignal;
}

// ---------------------------------------------------------------------------
// Per-command options
// ---------------------------------------------------------------------------

export interface BaseCallOptions {
  /** Override the client's working directory for this call. */
  cwd?: string;
  /** Extra environment variables for this call. Merged on top of the client env. */
  env?: Record<string, string | undefined>;
  /** Abort the underlying child process. */
  signal?: AbortSignal;
  /** Hard timeout in milliseconds. */
  timeoutMs?: number;
  /** Called for each line written to the child's stderr (where iterion logs go). */
  onStderrLine?: (line: string) => void;
}

/** Terminal statuses that may be promoted to thrown errors. */
export type ThrowableStatus = "failed" | "cancelled" | "paused_waiting_human";

export interface RunOptions extends BaseCallOptions {
  /** `--var key=value` overrides. */
  vars?: Record<string, string>;
  /** `--recipe` JSON file path. */
  recipe?: string;
  /** Explicit run ID; otherwise the engine generates one. */
  runId?: string;
  /** `--store-dir`. */
  storeDir?: string;
  /** `--timeout`. Numbers are interpreted as milliseconds, strings are passed through (e.g. "30s", "5m"). */
  timeout?: number | string;
  /** `--log-level`. */
  logLevel?: "error" | "warn" | "info" | "debug" | "trace";
  /**
   * Throw on the listed terminal statuses instead of returning the result.
   * The default behaviour is: throw on `failed`, return on `paused_waiting_human` / `cancelled` / `finished`.
   */
  throwOn?: ThrowableStatus[];
}

export interface ResumeOptions extends BaseCallOptions {
  /** Run to resume. Required. */
  runId: string;
  /** Workflow file (.iter). Required (the CLI requires `--file`). */
  file: string;
  /** `--store-dir`. */
  storeDir?: string;
  /** Pre-built JSON answers file path. Mutually compatible with `answers`. */
  answersFile?: string;
  /**
   * Answer map. Strings travel via `--answer key=value`; non-string
   * values are written to a temp `--answers-file`.
   */
  answers?: Record<string, JsonValue>;
  /** `--log-level`. */
  logLevel?: "error" | "warn" | "info" | "debug" | "trace";
  /** `--force`: allow resume after the workflow source has changed. */
  force?: boolean;
  /** Same semantics as `RunOptions.throwOn`. */
  throwOn?: ThrowableStatus[];
}

export interface InspectOptions extends BaseCallOptions {
  runId?: string;
  storeDir?: string;
  events?: boolean;
  full?: boolean;
}

export interface DiagramOptions extends BaseCallOptions {
  view?: "compact" | "detailed" | "full";
}

export interface ReportOptions extends BaseCallOptions {
  runId: string;
  storeDir?: string;
  output?: string;
}

export type ValidateOptions = BaseCallOptions;

export interface InitOptions extends BaseCallOptions {
  dir?: string;
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

export class IterionClient {
  // Cache the in-flight resolution promise (not just the resolved
  // string) so concurrent first calls share the same fs work.
  private binaryPromise: Promise<string> | null = null;

  constructor(private readonly opts: IterionClientOptions = {}) {}

  /** Resolve and cache the iterion binary path. */
  resolveBinary(): Promise<string> {
    if (!this.binaryPromise) {
      this.binaryPromise = resolveBinary({
        binPath: this.opts.binPath,
        env: this.mergedEnv(),
      }).catch((err) => {
        // Don't memoize a rejection — let the next caller retry.
        this.binaryPromise = null;
        throw err;
      });
    }
    return this.binaryPromise;
  }

  // ---- Commands -----------------------------------------------------------

  async version(callOpts: BaseCallOptions = {}): Promise<VersionResult> {
    const args = ["version"];
    const r = await this.invoke(args, callOpts);
    this.assertExit(r, args, "version");
    return { version: r.stdout.trim() };
  }

  async validate(file: string, callOpts: ValidateOptions = {}): Promise<ValidateResult> {
    const args = ["--json", "validate", file];
    const r = await this.invoke(args, callOpts);
    // On validation failure the CLI exits non-zero AND writes a second
    // `{"error":"validation failed"}` envelope after the structured
    // ValidateResult JSON (cmd/iterion/main.go's outer error handler).
    // Use parseFirstJSON so the structured envelope (which carries the
    // diagnostics — the entire reason validate() exists) is what we
    // return to the caller.
    try {
      const result = parseFirstJSON<unknown>(r.stdout, this.parseCtx(r, args));
      if (isValidateResult(result)) {
        return result;
      }
      throw new Error("validate output did not match the expected envelope");
    } catch {
      throw buildInvocationError(
        `iterion validate exited with code ${r.exitCode}`,
        r.bin,
        args,
        r,
      );
    }
  }

  async init(callOpts: InitOptions = {}): Promise<void> {
    const args = ["init"];
    if (callOpts.dir) args.push(callOpts.dir);
    const r = await this.invoke(args, callOpts);
    this.assertExit(r, args, "init");
  }

  async diagram(file: string, callOpts: DiagramOptions = {}): Promise<DiagramResult> {
    const args = ["--json", "diagram", file];
    if (callOpts.view) args.push("--view", callOpts.view);
    return this.invokeJSON<DiagramResult>(args, callOpts, "diagram");
  }

  async inspect(callOpts: InspectOptions = {}): Promise<InspectResult> {
    const args = ["--json", "inspect"];
    if (callOpts.runId) args.push("--run-id", callOpts.runId);
    const storeDir = this.resolveStoreDir(callOpts.storeDir);
    if (storeDir) args.push("--store-dir", storeDir);
    if (callOpts.events) args.push("--events");
    if (callOpts.full) args.push("--full");
    return this.invokeJSON<InspectResult>(args, callOpts, "inspect");
  }

  async report(callOpts: ReportOptions): Promise<ReportResult> {
    const args = ["--json", "report", "--run-id", callOpts.runId];
    const storeDir = this.resolveStoreDir(callOpts.storeDir);
    if (storeDir) args.push("--store-dir", storeDir);
    if (callOpts.output) args.push("--output", callOpts.output);
    const r = await this.invoke(args, callOpts);
    this.assertExit(r, args, "report");
    if (!r.stdout.trim()) {
      // `iterion report` may emit nothing on stdout when --output is set.
      return { run_id: callOpts.runId, output: callOpts.output };
    }
    return parseJSON<ReportResult>(r.stdout, this.parseCtx(r, args));
  }

  async run(file: string, callOpts: RunOptions = {}): Promise<RunResult> {
    const args = ["--json", "run", file];
    if (callOpts.vars) {
      for (const [k, v] of Object.entries(callOpts.vars)) {
        args.push("--var", `${k}=${v}`);
      }
    }
    if (callOpts.recipe) args.push("--recipe", callOpts.recipe);
    if (callOpts.runId) args.push("--run-id", callOpts.runId);
    const storeDir = this.resolveStoreDir(callOpts.storeDir);
    if (storeDir) args.push("--store-dir", storeDir);
    if (callOpts.timeout !== undefined) {
      args.push("--timeout", formatTimeout(callOpts.timeout));
    }
    if (callOpts.logLevel) args.push("--log-level", callOpts.logLevel);
    // Library callers should never end up at an interactive TTY prompt.
    args.push("--no-interactive");

    const r = await this.invoke(args, callOpts);
    return this.handleRunResultOutput<RunResult>(r, args, callOpts.throwOn);
  }

  async resume(callOpts: ResumeOptions): Promise<ResumeResult> {
    const args = ["--json", "resume", "--run-id", callOpts.runId, "--file", callOpts.file];
    const storeDir = this.resolveStoreDir(callOpts.storeDir);
    if (storeDir) args.push("--store-dir", storeDir);
    if (callOpts.logLevel) args.push("--log-level", callOpts.logLevel);
    if (callOpts.force) args.push("--force");
    if (callOpts.answersFile) args.push("--answers-file", callOpts.answersFile);

    const { flagAnswers, fileAnswers } = callOpts.answers
      ? partitionAnswers(callOpts.answers)
      : { flagAnswers: {}, fileAnswers: {} };
    for (const [k, v] of Object.entries(flagAnswers)) {
      args.push("--answer", `${k}=${v}`);
    }

    const hasFileAnswers = Object.keys(fileAnswers).length > 0;
    if (hasFileAnswers && callOpts.answersFile) {
      // The CLI loads --answers-file first then applies --answer overrides
      // (only string values), so non-string answers passed alongside an
      // explicit file would be silently dropped. Surface the conflict.
      throw new Error(
        "Non-string answers cannot be combined with an explicit answersFile. " +
          "Either pre-merge them into the file, or pass everything via `answers`.",
      );
    }

    let cleanup: (() => Promise<void>) | undefined;
    try {
      if (hasFileAnswers) {
        const written = await writeAnswersFile(fileAnswers);
        cleanup = written.cleanup;
        args.push("--answers-file", written.path);
      }
      const r = await this.invoke(args, callOpts);
      return this.handleRunResultOutput<ResumeResult>(r, args, callOpts.throwOn);
    } finally {
      if (cleanup) await cleanup();
    }
  }

  // ---- Event streaming ----------------------------------------------------

  /**
   * Tail the events.jsonl for a run. Reads directly from the store
   * (no child process). With `follow: true` the iterator stays open
   * until the supplied AbortSignal fires.
   */
  events(runId: string, opts: TailEventsOptions = {}): AsyncIterable<Event> {
    return tailEvents(runId, {
      ...opts,
      storeDir: this.resolveStoreDir(opts.storeDir),
      signal: opts.signal ?? this.opts.signal,
    });
  }

  // ---- Store helpers ------------------------------------------------------

  loadRun(runId: string, opts: { storeDir?: string } = {}): Promise<Run> {
    return storeLoadRun(runId, { storeDir: this.resolveStoreDir(opts.storeDir) });
  }

  loadInteraction(
    runId: string,
    interactionId: string,
    opts: { storeDir?: string } = {},
  ): Promise<Interaction> {
    return storeLoadInteraction(runId, interactionId, {
      storeDir: this.resolveStoreDir(opts.storeDir),
    });
  }

  loadArtifact(
    runId: string,
    nodeId: string,
    version?: number,
    opts: { storeDir?: string } = {},
  ): Promise<Artifact> {
    return storeLoadArtifact(runId, nodeId, version, {
      storeDir: this.resolveStoreDir(opts.storeDir),
    });
  }

  listRuns(opts: { storeDir?: string } = {}): Promise<string[]> {
    return storeListRuns({ storeDir: this.resolveStoreDir(opts.storeDir) });
  }

  // ---- Internals ----------------------------------------------------------

  private resolveStoreDir(perCall: string | undefined): string | undefined {
    return perCall ?? this.opts.storeDir;
  }

  private async invoke(
    args: readonly string[],
    callOpts: BaseCallOptions,
  ): Promise<InvokeResult> {
    const bin = await this.resolveBinary();
    const result = await execIterion(bin, args, this.execOptions(callOpts));
    return { ...result, bin };
  }

  private async invokeJSON<T>(
    args: readonly string[],
    callOpts: BaseCallOptions,
    label: string,
  ): Promise<T> {
    const r = await this.invoke(args, callOpts);
    this.assertExit(r, args, label);
    return parseJSON<T>(r.stdout, this.parseCtx(r, args));
  }

  private assertExit(r: InvokeResult, args: readonly string[], label: string): void {
    if (r.exitCode !== 0) {
      throw buildInvocationError(
        `iterion ${label} exited with code ${r.exitCode}`,
        r.bin,
        args,
        r,
      );
    }
  }

  private parseCtx(r: InvokeResult, args: readonly string[]) {
    return { bin: r.bin, args, stderr: r.stderr, exitCode: r.exitCode };
  }

  private execOptions(callOpts: BaseCallOptions): ExecOptions {
    return {
      cwd: callOpts.cwd ?? this.opts.cwd,
      env: this.mergedEnv(callOpts.env),
      signal: callOpts.signal ?? this.opts.signal,
      timeoutMs: callOpts.timeoutMs,
      onStderrLine: callOpts.onStderrLine,
    };
  }

  private mergedEnv(extra?: Record<string, string | undefined>): NodeJS.ProcessEnv {
    const merged: NodeJS.ProcessEnv = { ...process.env };
    applyEnvOverrides(merged, this.opts.env);
    applyEnvOverrides(merged, extra);
    return merged;
  }

  private handleRunResultOutput<T extends RunResult | ResumeResult>(
    r: InvokeResult,
    args: readonly string[],
    throwOn: ThrowableStatus[] | undefined,
  ): T {
    // The CLI emits JSON to stdout for run/resume regardless of exit code
    // for the four "engine ran" statuses (finished / paused / failed /
    // cancelled — see pkg/cli/run.go and pkg/cli/resume.go). For pre-
    // engine failures (parse error, missing file, unknown recipe, etc.)
    // the CLI bails out before assigning a status and the outer error
    // handler in cmd/iterion/main.go writes a bare `{"error":"..."}`
    // envelope to stdout instead.
    //
    // For `failed` and `cancelled` statuses the CLI ALSO double-writes:
    // pkg/cli/run.go:258-267 / pkg/cli/resume.go:182-191 emit the
    // structured envelope and then `return err`, which causes
    // cmd/iterion/main.go:46-52 to write a SECOND bare `{"error":"..."}`
    // envelope on top of the structured one. We must therefore parse
    // ONLY the first JSON value (parseFirstJSON), not the entire stdout
    // (which would JSON.parse-fail and silently lose the structured
    // status envelope).
    //
    // We detect three buckets:
    //   1. Empty stdout — catastrophic spawn failure.
    //   2. Status-less envelope (`{error: "..."}`) — pre-engine failure;
    //      surface as IterionRuntimeError / IterionInvocationError rather
    //      than silently returning a header-less RunResult.
    //   3. Status-bearing envelope — the normal contract; honour throwOn.
    const trimmed = r.stdout.trim();
    if (!trimmed) {
      const runtimeErr = parseRuntimeError(r.stderr);
      if (runtimeErr) throw runtimeErr;
      throw new IterionInvocationError(
        `iterion exited with code ${r.exitCode} and produced no output`,
        r.bin,
        args,
        r.exitCode,
        r.stdout,
        r.stderr,
      );
    }

    const result = parseFirstJSON<T>(trimmed, this.parseCtx(r, args));
    const status = result.status;

    if (!status) {
      // Pre-engine error: the CLI emitted only the global `{error: …}`
      // envelope. Don't return this as a typed RunResult — callers would
      // get `result.run_id === undefined` and silently lose the failure.
      const envelope = result as unknown as { error?: unknown };
      const envelopeMessage =
        typeof envelope.error === "string" && envelope.error.trim() !== ""
          ? envelope.error
          : undefined;
      const runtimeErr = parseRuntimeError(r.stderr);
      if (runtimeErr) throw runtimeErr;
      throw new IterionInvocationError(
        envelopeMessage
          ? `iterion ${args[1]} failed: ${envelopeMessage}`
          : `iterion ${args[1]} produced no status field (exit code ${r.exitCode})`,
        r.bin,
        args,
        r.exitCode,
        r.stdout,
        r.stderr,
      );
    }

    const shouldThrow = throwOn
      ? (s: ThrowableStatus) => throwOn.includes(s)
      : (s: ThrowableStatus) => s === "failed";

    if (status === "failed" && shouldThrow("failed")) {
      const runtimeErr = parseRuntimeError(r.stderr);
      if (runtimeErr) throw runtimeErr;
      throw new IterionRuntimeError(
        "EXECUTION_FAILED",
        result.error ?? `iterion ${args[1]} failed`,
        undefined,
        undefined,
        r.stderr,
      );
    }

    if (status === "cancelled" && shouldThrow("cancelled")) {
      throw new IterionRuntimeError(
        "CANCELLED",
        "run cancelled",
        undefined,
        undefined,
        r.stderr,
      );
    }

    if (status === "paused_waiting_human" && shouldThrow("paused_waiting_human")) {
      throw new IterionRunPausedSignal(
        result.run_id,
        result.interaction_id,
        result.node_id,
        result.questions,
      );
    }

    return result;
  }
}

interface InvokeResult extends ExecResult {
  bin: string;
}

function applyEnvOverrides(
  target: NodeJS.ProcessEnv,
  overrides: Record<string, string | undefined> | undefined,
): void {
  if (!overrides) return;
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) {
      delete target[k];
    } else {
      target[k] = v;
    }
  }
}

function formatTimeout(t: number | string): string {
  return typeof t === "string" ? t : `${Math.max(0, Math.floor(t))}ms`;
}

function isValidateResult(value: unknown): value is ValidateResult {
  if (!value || typeof value !== "object") return false;
  const candidate = value as { file?: unknown; valid?: unknown };
  return typeof candidate.valid === "boolean" && typeof candidate.file === "string";
}
