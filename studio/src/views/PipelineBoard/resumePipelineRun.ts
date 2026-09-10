import { resumeRun } from "@/api/runs";
import { isForceResumeRequiredError } from "@/api/runs/lifecycle";

// Operator-paused runs normally resume against the source they started with.
// If that source has changed, make the potentially surprising force-resume an
// explicit second step instead of leaving the board at an opaque HTTP 400.
export async function resumePipelineRun(
  runId: string,
  confirmUpdatedWorkflow: () => Promise<boolean>,
): Promise<void> {
  try {
    await resumeRun(runId, {});
  } catch (error) {
    if (!isForceResumeRequiredError(error)) throw error;
    if (!(await confirmUpdatedWorkflow())) return;
    await resumeRun(runId, { force: true });
  }
}
