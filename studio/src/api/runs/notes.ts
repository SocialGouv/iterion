// Per-run operator notes: freeform text annotations that persist with a
// run and are visible to the team (GET/POST /api/runs/:id/notes). See the
// Go RunNoteStore seam (pkg/store/notes.go) for the persistence shape.

import { request } from "./client";
import type { RunNote } from "./types";

// listNotes returns the run's operator notes in chronological (ascending
// seq) order. Empty for a run with no notes / a store that predates the
// feature.
export async function listNotes(
  runId: string,
  opts?: { signal?: AbortSignal },
): Promise<RunNote[]> {
  const res = await request<{ notes: RunNote[] }>(
    `/runs/${encodeURIComponent(runId)}/notes`,
    { signal: opts?.signal },
  );
  return res.notes ?? [];
}

// addNote appends a freeform note to the run and returns the created
// note (with its assigned seq + server timestamp). author is optional —
// the server defaults it from the authenticated caller.
export async function addNote(
  runId: string,
  body: string,
  opts?: { author?: string },
): Promise<RunNote> {
  return request<RunNote>(`/runs/${encodeURIComponent(runId)}/notes`, {
    method: "POST",
    body: JSON.stringify({ body, author: opts?.author }),
  });
}
