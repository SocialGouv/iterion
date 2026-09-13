import { errorMessage } from "@/lib/errorHints";

// The resume hash guard is deliberately a server verdict: only the server has
// resolved the run's persisted source origin and the workflow currently in
// force. UI callers use this narrow classifier to offer the explicit `force`
// escape hatch without swallowing unrelated HTTP 400s.
export function isResumeSourceChangedError(error: unknown): boolean {
  return /workflow source has changed|source has changed/i.test(
    errorMessage(error),
  );
}
