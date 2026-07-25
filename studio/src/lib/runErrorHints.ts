// Run-level error code → hint mapping for the RunHeader ErrorHintRow.
// Distinct from lib/errorHints.ts (which exports a different errorHint(err)
// that turns raw thrown values into a friendly title/hint pair for toasts);
// keep the two names disambiguated to avoid shadowing.

import type { RunHeader as RunHeaderType } from "@/api/runs";

export function parseErrorCode(err: string): string {
  // Matches the "[CODE] …" prefix produced by RuntimeError.Error().
  const m = err.match(/^\s*\[([A-Z_]+)\]/);
  return m ? m[1]! : "";
}

export function runErrorHint(code: string, run: RunHeaderType): string | null {
  switch (code) {
    case "BUDGET_EXCEEDED":
      return "Open Resume, expand Budget overrides to raise max_cost_usd / max_tokens / max_duration / max_iterations past the exhausted cap, then click Resume — the run continues from the failing node.";
    case "RATE_LIMITED":
      return "Wait a few minutes for the provider rate limit to clear, then resume — the engine retries from the failed node.";
    case "LOOP_EXHAUSTED":
      return "Raise the loop's `(N)` count in the workflow, or accept the partial output and let the run finish.";
    case "CONTEXT_LENGTH_EXCEEDED":
      return "Lower the per-node compaction `ratio:` (or enable compaction) and resume — the conversation overflowed the model's window.";
    case "WORKSPACE_SAFETY":
      return "Re-author the workflow so at most one branch holds a worktree-touching tool — multiple mutating branches collided.";
    case "TIMEOUT":
      return "Increase `max_duration` in the workflow's `budget:` block (or set a per-node timeout), then resume.";
    case "TOOL_FAILED_PERMANENT":
      return "Inspect the failing tool call in the Tools tab, fix the input or the tool itself, then resume.";
    case "SCHEMA_VALIDATION":
      return "The workflow source changed since launch. Open Resume, tick Force (skip hash check), then click Resume — or fix the agent's prompt / relax the schema first.";
    case "RESUME_INVALID":
      return "The workflow source changed since launch. Open Resume, tick Force (skip hash check), then click Resume.";
    case "NETWORK_TRANSIENT":
      return "Resume to retry the LLM API call — a transient network blip interrupted the request.";
    default:
      // Suppress the hint for raw panics / stacktraces; otherwise point
      // the operator at the Events tab so they at least know where to
      // look next.
      if (run.error?.startsWith("panic:")) return null;
      // Sandbox start failures (docker postCreate, image pull races,
      // missing binaries inside the container) are recoverable from the
      // operator's side once the underlying infra issue is resolved.
      // Point at the dispatcher state docs so the operator can verify
      // docker is up, the image is reachable, and credentials mounted.
      if (run.error?.includes("sandbox: start") || run.error?.includes("postCreate")) {
        return "Sandbox start failed — verify `docker info` works, the image is reachable, and (for sandboxed claw) the `iterion` binary is on PATH inside the container. Resume retries the same sandbox bootstrap.";
      }
      return "Open the Events tab for the failing step's logs, then resume after addressing the root cause.";
  }
}
