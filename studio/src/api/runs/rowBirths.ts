// Run rows carry the run's birth (birth_id). The page notes it when the row
// arrives, so the view opened from that row expects THAT run: a run replaced
// between the listing and the navigation is refused by the server (409, a
// scope_lost socket) and shown lost, never shown in the listed run's place.
// A row never marks a run lost and never replaces a birth a snapshot taught
// the page, and a row of a request sent before another's landing after it
// changes nothing (@/lib/lostRuns noteRunRowBirth): a listing can land late,
// or see a run recreated that no view watches.

import { noteRunRowBirth } from "@/lib/lostRuns";

// noteRunBirths notes every row's birth under store — the ?store= of the
// request a view of that row sends ("" for the daemon's own store; a foreign
// store's rows teach nothing) — seq being the rows' request number
// (beginRowRequest, taken before it was sent), and returns rows.
export function noteRunBirths<T extends { id: string; birth_id?: string }>(store: string, rows: T[], seq: number): T[] {
  for (const row of rows) noteRunRowBirth(store, row.id, row.birth_id, seq);
  return rows;
}
