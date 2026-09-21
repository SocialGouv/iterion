import * as api from "@/api/client";
import type { IterDocument, UnitInfo } from "@/api/types";

/**
 * The slice of a document store needed to load a bot / bundled example into
 * it. Both the app-level singleton store (Toolbar, CanvasEmpty) and a per-tab
 * store obtained from `getOrCreateDocumentStore(tabId).getState()` satisfy
 * this shape, so all three example-open entry points share one path.
 */
export interface ExampleTargetStore {
  setDocument: (document: IterDocument) => void;
  setDiagnostics: (diagnostics: string[]) => void;
  setCurrentSource: (source: string | null) => void;
  setCurrentFilePath: (path: string | null) => void;
  /** Set AFTER the path, which clears it: the document of an example that
   *  does not parse is a salvage, and writing it back is refused. */
  setSalvaged: (salvaged: boolean) => void;
  /** A bot in several files binds its unit: the document is the merged
   *  program, its source view is read-only, a save presents the revision.
   *  Bound AFTER the path, which clears it. */
  setUnit: (unit: UnitInfo | null) => void;
  markSaved: () => void;
}

/**
 * Load a first-class bot / bundled example by its relative name (e.g.
 * `"feature-dev/main.bot"`) and apply it to `store`.
 *
 * Binds `currentFilePath` to the path the server names — a file inside the
 * workspace — else to `bots/<name>`, where a save of the one program lands,
 * BEFORE `markSaved()`, so the freshly-loaded state is the clean saved
 * baseline: bound and saved, it is what the Run button launches by path
 * (`launchRefusal`), unless it is a salvage. Keeps the example's `source` +
 * `diagnostics` so Save and cloud-mode resume work without a re-open, and
 * binds the unit of a bot in several files.
 *
 * Throws if the load fails; callers decide how to surface that. Returns the
 * loaded result.
 */
export async function openExampleIntoStore(name: string, store: ExampleTargetStore) {
  const result = await api.loadExample(name);
  store.setDocument(result.document);
  store.setDiagnostics(result.diagnostics);
  store.setCurrentSource(result.source);
  // The path first: setting it clears the unit and the salvage flag, so both
  // are set after it. An example that does not parse still names its file —
  // the editor is about that file — and is marked a SALVAGE instead, which
  // is what refuses the write.
  store.setCurrentFilePath(result.path ?? `bots/${name}`);
  store.setSalvaged(result.bindable === false);
  store.setUnit(result.unit ?? null);
  store.markSaved();
  return result;
}
