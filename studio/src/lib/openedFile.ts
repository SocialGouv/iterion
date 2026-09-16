import type { IterDocument, UnitInfo } from "@/api/types";

/** What `/api/files/open` answers, as far as the store cares. */
export interface OpenedFile {
  source: string;
  document: IterDocument;
  diagnostics: string[];
  /** Absent when the file does not parse. */
  path?: string;
  unit?: UnitInfo;
}

/** The slice of the document store an opened file writes to. */
export interface OpenedFileTargetStore {
  setDocument: (d: IterDocument) => void;
  setDiagnostics: (d: string[]) => void;
  setCurrentSource: (s: string) => void;
  setCurrentFilePath: (p: string | null) => void;
  setUnit: (u: UnitInfo | null) => void;
  markSaved: () => void;
}

/**
 * Applies an opened file to a document store.
 *
 * A file the parser could not read whole comes back with NO path: the
 * document is then what the parser salvaged — the file minus the region it
 * could not read — and binding it would make the next Save write that back
 * over what the author wrote. Unbound, the text and the diagnostics are still
 * shown, and a Save has to ask where.
 *
 * The path is set before the unit, because setting it clears the unit.
 *
 * One function for every flow that applies an opened file — the picker, the
 * tab host, the file watcher's reload, the assistant's post-write reload.
 * The rule is a single decision, and a second copy of it is how one site keeps
 * binding: the watcher and the assistant reload were exactly that, and an
 * external write to the file was enough to re-bind the salvage with no user
 * action at all.
 *
 * Blind spot, stated rather than implied: a new site that hand-rolls the same
 * setters is invisible here. There is no guard for it — one over the source
 * text would certify a spelling — so the check is this function's call sites.
 */
export function applyOpenedFile(result: OpenedFile, store: OpenedFileTargetStore) {
  store.setDocument(result.document);
  store.setDiagnostics(result.diagnostics);
  store.setCurrentFilePath(result.path ?? null);
  store.setCurrentSource(result.source);
  store.setUnit(result.unit ?? null);
  store.markSaved();
}
