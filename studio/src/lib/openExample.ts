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
 * workspace that parses clean — to nothing when the server says the file is
 * not bindable (it does not parse: a save then asks where, and Run stays
 * disabled, which is honest for a file that cannot run), else to
 * `bots/<name>`, where a save of the one program lands; BEFORE `markSaved()`
 * so the freshly-loaded state is the clean saved baseline AND the Run button
 * enables immediately (otherwise it stays disabled with "Save the workflow
 * first to launch a run"). Keeps the example's `source` + `diagnostics` so
 * Save and cloud-mode resume work without a re-open, and binds the unit of
 * a bot in several files.
 *
 * Throws if the load fails; callers decide how to surface that. Returns the
 * loaded result.
 */
export async function openExampleIntoStore(name: string, store: ExampleTargetStore) {
  const result = await api.loadExample(name);
  store.setDocument(result.document);
  store.setDiagnostics(result.diagnostics);
  store.setCurrentSource(result.source);
  // The path first: setting it clears the unit, so the unit is bound after
  // it. A file inside the workspace that parses clean names the path the
  // studio opens and saves it by; one that does not parse binds NOTHING —
  // bots/<name> would name that very file in the default layout, and a
  // save would replace what the author wrote with what the parser kept;
  // anything else binds bots/<name>, where a save of the one program lands.
  store.setCurrentFilePath(result.path ?? (result.bindable === false ? null : `bots/${name}`));
  store.setUnit(result.unit ?? null);
  store.markSaved();
  return result;
}
