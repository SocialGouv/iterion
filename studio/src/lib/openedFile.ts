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
 * One function for both open flows (the picker and the tab host): the rule is
 * a single decision, and a second copy of it is how one site keeps binding.
 */
export function applyOpenedFile(result: OpenedFile, store: OpenedFileTargetStore) {
  store.setDocument(result.document);
  store.setDiagnostics(result.diagnostics);
  store.setCurrentFilePath(result.path ?? null);
  store.setCurrentSource(result.source);
  store.setUnit(result.unit ?? null);
  store.markSaved();
}
