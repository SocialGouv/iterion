import type { IterDocument, ReadRefusal } from "@/api/types";

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
 * text while salvaged) and the assistant's context snapshot — which is the
 * text Copi writes its proposals from, so it withholds a salvage the
 * server's READER made (captureActiveEditorDocument: no source, the refusal
 * said), as it withholds a document past its size limit.
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
export function salvageRefusal(state: { salvaged: boolean; unit?: unknown; readRefusal?: ReadRefusal | null }): string | null {
  if (!state.salvaged) return null;
  // A salvage the SERVER's reader made — the read budget the bot's files
  // share, a file's size cap, or a file that is no text it reads (not
  // UTF-8) — is no text to repair here: the remedy is the server's dial, or
  // the file's encoding changed outside the studio, which the refusal names.
  // Told before the two branches below, which are about a file that did not
  // parse and would send the author to repair it in the Source view.
  if (state.readRefusal) return readRefusalSentence(state.readRefusal);
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

/** What every refusing site says of a salvage the server's reader made:
 *  the cause, positioned, and its remedy — then reopen. */
export function readRefusalSentence(r: ReadRefusal): string {
  return `The server did not read this bot whole (${r.message}), so the canvas holds only what was read of it — saving would write the rest away.${r.hint ? ` ${r.hint}` : ""} Then reopen it.`;
}

/** A parse the server's reader refused whole carries no program to apply:
 *  the error every applying site shows in place of applying it. */
export class ReadRefusalError extends Error {
  constructor(r: ReadRefusal) {
    super(`The text is not read whole (${r.message}), so nothing was applied.${r.hint ? ` ${r.hint}` : ""}`);
    this.name = "ReadRefusalError";
  }
}

/** Throws ReadRefusalError for an answer that carries a read refusal — the
 *  check an applying site makes before it touches the store. */
export function refuseReadRefusal(parsed: { read_refusal?: ReadRefusal }) {
  if (parsed.read_refusal) throw new ReadRefusalError(parsed.read_refusal);
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
 * calls this AFTER setting the path, which clears what this sets, and hands
 * the store as it is THEN.
 *
 * A salvage the server's READER made (store.readRefusal: a read budget, a
 * size cap, a file that is no text it reads) is the one a whole answer does
 * not end. Nothing here read its files — not the author, not Copi, whose
 * context withholds them —
 * so an answer applied over it is not a repair of the file but a text that
 * would replace what nobody read. The canvas takes the answer; the salvage
 * and its refusal stay, and only reopening the file once the reader reads it
 * whole ends them. `readRefusal` is the store's as it is at the call, which
 * is why every site has to say it.
 */
export function applyParsedSource(
  parsed: { document: IterDocument; bindable?: boolean; read_refusal?: ReadRefusal },
  store: {
    setDocument: (d: IterDocument) => void;
    setSalvaged: (salvaged: boolean, readRefusal?: ReadRefusal | null) => void;
    readRefusal: ReadRefusal | null;
  },
) {
  // A text the server's reader refused whole parses to nothing of it: put
  // over the document this tab holds, it would replace the program with an
  // empty one. Refused before the store is touched.
  refuseReadRefusal(parsed);
  store.setDocument(parsed.document);
  if (store.readRefusal) {
    store.setSalvaged(true, store.readRefusal);
    return;
  }
  store.setSalvaged(parsed.bindable === false);
}
