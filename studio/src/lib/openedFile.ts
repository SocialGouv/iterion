import type { IterDocument, UnitInfo } from "@/api/types";

/** What `/api/files/open` answers, as far as the store cares. */
export interface OpenedFile {
  source: string;
  document: IterDocument;
  diagnostics: string[];
  path?: string;
  unit?: UnitInfo;
  /** False when the parser could not read the file whole: `document` is then
   *  what it salvaged. Absent reads as true — an older server, and the
   *  answers a client builds for a program it holds itself. */
  bindable?: boolean;
}

/** The slice of the document store an opened file writes to. */
export interface OpenedFileTargetStore {
  setDocument: (d: IterDocument) => void;
  setDiagnostics: (d: string[]) => void;
  setCurrentSource: (s: string) => void;
  setCurrentFilePath: (p: string | null) => void;
  setSalvaged: (salvaged: boolean) => void;
  setUnit: (u: UnitInfo | null) => void;
  markSaved: () => void;
}

/**
 * Applies an opened file to a document store.
 *
 * A file the parser could not read whole still NAMES its path — this tab is
 * about that file, and every surface that asks which one (the watcher, the
 * tab binding, the validation scope, the bundle drawer, the assistant's
 * perimeter) goes on working. What comes back with it is the word that the
 * document is a SALVAGE, which forbids writing it: a save would put the
 * parser's reading over what the author wrote.
 *
 * The flag is set after the path, because setting the path clears it.
 *
 * One function for every flow that applies an opened file — the picker, the
 * tab host, the file watcher's reload, the assistant's post-write reload —
 * so the rule is one decision. A second copy of it is how one site keeps
 * writing: the watcher and the assistant reload were exactly that.
 *
 * Blind spot, stated rather than implied: a new site that hand-rolls the same
 * setters is invisible here. There is no guard for it — one over the source
 * text would certify a spelling — so the check is this function's call sites,
 * and the three that WRITE, which share `salvageRefusal`.
 */
export function applyOpenedFile(result: OpenedFile, store: OpenedFileTargetStore) {
  store.setDocument(result.document);
  store.setDiagnostics(result.diagnostics);
  store.setCurrentFilePath(result.path ?? null);
  store.setSalvaged(result.bindable === false);
  store.setCurrentSource(result.source);
  store.setUnit(result.unit ?? null);
  store.markSaved();
}
