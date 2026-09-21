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
 * by the same words; then error diagnostics, which the server would refuse
 * with the same errors after the form was filled in. Warnings do not refuse.
 */
export function launchRefusal(s: LaunchGateState): string | null {
  if (!s.document || pristineBuffer(s)) {
    return "Write or open a workflow first to launch a run";
  }
  const salvage = salvageRefusal(s);
  if (salvage) return salvage;
  const errors = s.diagnostics.length;
  if (errors > 0) {
    return `Fix the ${errors} error${errors === 1 ? "" : "s"} in the Diagnostics panel before launching`;
  }
  return null;
}
