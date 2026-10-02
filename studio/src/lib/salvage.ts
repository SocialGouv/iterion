import type { IterDocument } from "@/api/types";

/**
 * The one reason a document may not be written back to the file it names.
 *
 * `salvaged` says the parser could not read the file whole: the document is
 * the file MINUS the region it could not read. Writing it back replaces what
 * the author wrote with the parser's reading of it, silently and totally for
 * that region — the loss #1251 is about.
 *
 * The refusal is on every site that hands the document out AS the program:
 * the three that write it (the toolbar's Save, the assistant's commit, Save
 * As), the two that export it (Download, Copy source) — a .bot on the
 * author's disk or in their clipboard is trusted the same way — and the
 * inline LAUNCH, which is the harshest: it loses no bytes, it runs a
 * workflow the author never wrote, at real cost, and the run's report gives
 * no sign of what is missing.
 *
 * It is never on the sites that ask which FILE this is: unbinding the path
 * to stop a write took the file's identity from every reader of it, which is
 * a second loss by another road.
 *
 * The class is the references to `unparse` — the SYMBOL, not the spelling.
 * Grepping `api.unparse(` missed the launch, which calls it as
 * `filesApi.unparse(storeDocument)` in a method chain. Two of the references
 * only display and are exempt: the Source view (which shows the file's own
 * text while salvaged) and the assistant's context snapshot.
 *
 * For a bot in ONE file it lifts by itself: a parse of the buffer that comes
 * back whole clears the flag, so repairing the text in the Source view makes
 * Save work again. That is the only way out, deliberately — the canvas cannot
 * restore a region the parser never read.
 *
 * For a bot in SEVERAL files it does not lift there at all. A save of one
 * re-derives the unit from the files as they are stored and refuses one that
 * does not load, so no buffer reaches a main that does not parse; the Source
 * view is read-only for it, and the flag lifts when the FILES parse. The
 * `state.unit` branch below says so, and says nothing about which control
 * repairs them — the control differs per twin and the cloud twin has none
 * (#1659).
 */
export function salvageRefusal(state: { salvaged: boolean; unit?: unknown }): string | null {
  if (!state.salvaged) return null;
  // Three sentences were written for this branch and two named a CONTROL:
  // the files drawer, which renders only for a cloud path; then the Source
  // view, whose Apply answers 200 and whose Save then refuses. The control
  // that works differs per twin and on the cloud twin there is none — the
  // bundle files drawer sends `main.bot` to the canvas, which is the tab
  // that is refusing.
  //
  // So this names the CONSTRAINT, which is one fact, true on both twins and
  // checkable: a bot in several files is saved from its files as they are
  // stored, and a main that does not parse has to be repaired there. Where
  // "there" is belongs to the surface that has it, not to this sentence.
  if (state.unit) {
    return "This bot's main did not parse, so the canvas holds only what could be read of it — saving would drop the rest. A bot in several files is saved from its files as they are stored, so nothing edited here can reach a main that does not parse: repair the main where this bot's files live, then reopen it.";
  }
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
