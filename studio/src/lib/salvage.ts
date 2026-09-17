import type { IterDocument } from "@/api/types";

/**
 * The one reason a document may not be written back to the file it names.
 *
 * `salvaged` says the parser could not read the file whole: the document is
 * the file MINUS the region it could not read. Writing it back replaces what
 * the author wrote with the parser's reading of it, silently and totally for
 * that region — the loss #1251 is about.
 *
 * The refusal is on the three sites that WRITE a document (the toolbar's
 * Save, the assistant's commit, Save As), never on the ones that ask which
 * file this is: unbinding the path to stop a write took the file's identity
 * away from every reader of it, which is a second loss by another road.
 *
 * It lifts by itself: a parse of the buffer that comes back whole clears the
 * flag, so repairing the text in the Source view makes Save work again. That
 * is the only way out, deliberately — the canvas cannot restore a region the
 * parser never read.
 */
export function salvageRefusal(state: { salvaged: boolean }): string | null {
  if (!state.salvaged) return null;
  return "This file did not parse, so the canvas holds only what could be read of it — saving would drop the rest. Repair it in the Source view and Apply.";
}

/**
 * Replaces the document with the parse of a FULL source, and with it the
 * verdict: text that parses whole makes the document the program again, so a
 * save may write it.
 *
 * The two travel together on purpose. Setting the document without the
 * verdict is how a repaired buffer stays unwritable for the rest of the
 * session; setting the verdict without the document is how a salvage becomes
 * writable. Every site that applies a parsed source goes through here — the
 * Source view's Apply, a draft bot applied to a tab, the assistant's
 * proposal, and Import.
 *
 * Import is the one that looks exempt and is not: it unbinds, and unbinding
 * clears the flag — which is the bug, not the protection. An unbound buffer
 * still has one write, Save As, and that is where a file missing the region
 * the parser could not read would land, under the name the author chose. It
 * calls this AFTER setting the path, which clears what this sets.
 */
export function applyParsedSource(
  parsed: { document: IterDocument; bindable?: boolean },
  store: { setDocument: (d: IterDocument) => void; setSalvaged: (salvaged: boolean) => void },
) {
  store.setDocument(parsed.document);
  store.setSalvaged(parsed.bindable === false);
}
