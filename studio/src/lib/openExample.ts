import * as api from "@/api/client";
import type { DocumentStore } from "@/store/document";
import { replaceDocument, type ReplaceOutcome } from "@/lib/replaceDocument";

/**
 * Load a first-class bot / bundled example by its relative name (e.g.
 * `"feature-dev/main.bot"`) and apply it to the tab's `store`. Every
 * example-open entry point (the Toolbar's picker, the empty canvas, the
 * recent-files panel) goes through here, and through `replaceDocument`: an
 * answer that lands after a newer replacement, or after the author edited,
 * does not land.
 *
 * Binds `currentFilePath` to the path the server names — a file inside the
 * workspace — else to `bots/<name>`, where a save of the one program lands,
 * BEFORE `markSaved()`, so the freshly-loaded state is the clean saved
 * baseline: bound and saved, it is what the Run button launches by path
 * (`launchRefusal`), unless it is a salvage. Keeps the example's `source` +
 * `diagnostics` so Save and cloud-mode resume work without a re-open, and
 * binds the unit of a bot in several files.
 *
 * Throws if the load fails; callers decide how to surface that.
 */
export async function openExampleIntoStore(name: string, store: DocumentStore): Promise<ReplaceOutcome> {
  return replaceDocument(store, name, () => api.loadExample(name), (result, s) => {
    s.setDocument(result.document);
    s.setDiagnostics(result.diagnostics);
    s.setCurrentSource(result.source);
    // The path first: setting it clears the unit and the salvage flag, so both
    // are set after it. An example that does not parse still names its file —
    // the editor is about that file — and is marked a SALVAGE instead, which
    // is what refuses the write.
    s.setCurrentFilePath(result.path ?? `bots/${name}`);
    s.setSalvaged(result.bindable === false);
    s.setUnit(result.unit ?? null);
    s.markSaved();
  });
}
