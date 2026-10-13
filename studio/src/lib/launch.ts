import type { IterDocument, UnitInfo } from "@/api/types";
import { salvageRefusal } from "@/lib/salvage";

/** The slice of the document store the launch gate reads. */
export interface LaunchGateState {
  document: IterDocument | null;
  currentFilePath: string | null;
  salvaged: boolean;
  unit: UnitInfo | null;
  diagnostics: string[];
  _generation: number;
  _savedGeneration: number;
}

/**
 * True for a buffer nothing was ever put in: bound to no file, and not
 * edited past the saved mark. The store initialises with a scaffold
 * document, so `document` alone never says "empty" — a fresh tab, File → New
 * and Start blank all hold that scaffold, saved. Offering it as an "unsaved
 * workflow" is how Run led to an empty launch view (#1326).
 */
export function pristineBuffer(s: {
  currentFilePath: string | null;
  _generation: number;
  _savedGeneration: number;
}): boolean {
  return s.currentFilePath === null && s._generation === s._savedGeneration;
}

/**
 * Why a bot in SEVERAL files may not be launched: one of its files does not
 * parse, so the program the server would read off those files is not the
 * merged document the canvas holds — the launch re-reads the raw source and
 * the parse 400s after the form was filled in.
 *
 * It is a fact about the FILES, answered per file by the server, and that
 * is the whole point: a unit whose main parses opens bindable, so no
 * salvage refuses it, and the diagnostics naming the broken fragment are
 * rewritten as soon as auto-validation answers on the merged document —
 * which is built from what parsed and validates clean.
 *
 * It is read from the file list the unit carries, which the per-file Apply
 * replaces with the answer's — and that answer's verdict is the STORED
 * files' (storedUnitVerdict): an apply has stored nothing for the refusal
 * to lift, and no save stores it either, since saveUnit refuses a bot
 * whose stored files do not load as one — which a file that does not parse
 * makes true, its diagnostics being part of that load's. So repairing the
 * fragment through the picker leaves the refusal standing: what lifts it is
 * the files on DISK changing, and the tab reading them again.
 *
 * The broken files are named in path order, not in the order the answer
 * lists them: the message is then the same for the same set of files,
 * whatever order they arrive in, and the error it quotes is always the same
 * one.
 */
function unitParseRefusal(unit: UnitInfo): string | null {
  const broken = unit.files
    .filter((f) => (f.parse_errors ?? 0) > 0)
    .sort((a, b) => (a.rel < b.rel ? -1 : a.rel > b.rel ? 1 : 0));
  if (broken.length === 0) return null;
  const errors = broken.reduce((n, f) => n + (f.parse_errors ?? 0), 0);
  const names = broken.map((f) => f.rel).join(", ");
  const first = broken.find((f) => f.parse_error)?.parse_error;
  const does = broken.length === 1 ? "that file does" : "those files do";
  return (
    `Fix the ${errors} error${errors === 1 ? "" : "s"} in ${names} before launching: ` +
    `a bot in several files is launched from its files as they are stored, and ${does} not parse` +
    `${first ? ` (${first})` : ""}.`
  );
}

/**
 * Why a bot in several files may not be launched when every one of them
 * parses: they do not load as ONE bot — an `import` naming no fragment, a
 * cycle, a declaration made in two files. `load_errors` counts the load's
 * whole diagnostics list, so it holds the loader's own errors AND every
 * file's parse errors, of which it is a superset; the merged document is
 * built from what loaded and validates clean, so this is the only fact that
 * survives the rewrite, and the launch refuses on exactly the list it is
 * taken from.
 */
function unitLoadRefusal(unit: UnitInfo): string | null {
  const errors = unit.load_errors ?? 0;
  if (errors === 0) return null;
  const first = unit.load_error;
  return (
    `Fix the ${errors} error${errors === 1 ? "" : "s"} in this bot's files before launching: ` +
    `a bot in several files is launched from its files as they are stored, and they do not load as one bot` +
    `${first ? ` (${first})` : ""}.`
  );
}

/**
 * Why the bot in several files behind an answer may not be launched, or null
 * when it can: the files the launch would read do not parse, or do not load
 * as one bot. The per-file verdict comes first — it names the file to
 * repair — and a file that does not parse makes both true.
 *
 * Exported for the launch view's `?file=` road, which fetches the unit
 * itself: the refusal is the same on every surface that offers the launch,
 * or one of them offers a form the server refuses once it is filled in.
 */
export function unitRefusal(unit: UnitInfo | null | undefined): string | null {
  if (!unit) return null;
  return unitParseRefusal(unit) ?? unitLoadRefusal(unit);
}

/**
 * Why the editor's document cannot be launched, or null when it can. One
 * rule for the two surfaces that offer the launch — the toolbar's Run button
 * (disabled, with this as its title) and the launch view's inline path (this
 * as its error) — so the button never leads to a view that has nothing to
 * launch or a launch the server refuses.
 *
 * The reasons are ordered from the most fundamental, since only one is
 * shown: a buffer with nothing in it; then a salvage, which is the certain
 * fact — auto-validation rewrites the diagnostics with the verdict on the
 * salvaged DOCUMENT, which may be clean — and which the launch view refuses
 * by the same words; then the unit's own verdict on the files — a file that
 * does not parse, or files that do not load as one bot — the same kind of
 * fact for a bot in several files, where the main parsing is not enough;
 * then error diagnostics, which the server would refuse with the same
 * errors after the form was filled in. Warnings do not refuse.
 */
export function launchRefusal(s: LaunchGateState): string | null {
  if (!s.document || pristineBuffer(s)) {
    return "Write or open a workflow first to launch a run";
  }
  const salvage = salvageRefusal(s);
  if (salvage) return salvage;
  const unitParse = unitRefusal(s.unit);
  if (unitParse) return unitParse;
  const errors = s.diagnostics.length;
  if (errors > 0) {
    return `Fix the ${errors} error${errors === 1 ? "" : "s"} in the Diagnostics panel before launching`;
  }
  return null;
}
