/**
 * @iterion/sdk — TypeScript SDK for the iterion workflow CLI.
 *
 * @example
 * ```ts
 * import { IterionClient } from "@iterion/sdk";
 *
 * const iterion = new IterionClient({ storeDir: ".iterion" });
 * const result = await iterion.run("examples/my_workflow.iter", {
 *   vars: { repo: "my-repo" },
 * });
 * if (result.status === "paused_waiting_human") {
 *   const resumed = await iterion.resume({
 *     runId: result.run_id,
 *     file: "examples/my_workflow.iter",
 *     answers: { approve: true },
 *   });
 * }
 * ```
 */

export { IterionClient } from "./client.js";
export type {
  BaseCallOptions,
  DiagramOptions,
  InitOptions,
  InspectOptions,
  IterionClientOptions,
  ReportOptions,
  ResumeOptions,
  RunOptions,
  ThrowableStatus,
  ValidateOptions,
} from "./client.js";

export {
  IterionBinaryNotFoundError,
  IterionError,
  IterionInvocationError,
  IterionRunPausedSignal,
  IterionRuntimeError,
  parseRuntimeError,
} from "./errors.js";

export { detectPlatform, resolveBinary } from "./binary.js";
export type {
  BinaryResolveOptions,
  PlatformDetectInput,
  PlatformTarget,
} from "./binary.js";

export { tailEvents } from "./events.js";
export type { TailEventsOptions } from "./events.js";

export {
  partitionAnswers,
  writeAnswersFile,
} from "./answers.js";
export type { AnswersFile } from "./answers.js";

export {
  listRuns,
  loadArtifact,
  loadInteraction,
  loadRun,
} from "./store.js";

export type {
  Artifact,
  Checkpoint,
  Diagnostic,
  DiagramResult,
  Event,
  EventType,
  InspectResult,
  InspectSingleResult,
  Interaction,
  JsonValue,
  ReportResult,
  ResumeResult,
  ResumeResultBase,
  ResumeResultCancelled,
  ResumeResultFailed,
  ResumeResultFinished,
  ResumeResultPaused,
  Run,
  RunResult,
  RunResultBase,
  RunResultCancelled,
  RunResultFailed,
  RunResultFinished,
  RunResultPaused,
  RunStatus,
  RuntimeErrorCode,
  ValidateResult,
  VersionResult,
} from "./types.js";
