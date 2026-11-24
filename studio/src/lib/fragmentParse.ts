import type { IterDocument, ReadRefusal } from "@/api/types";

/**
 * The apply-time detection #1650 (item 3) asks for.
 *
 * /api/parse answers a buffer that is not a valid standalone workflow — an
 * empty text, or a fragment of a bot in several files (declarations, no
 * `workflow`) — with a 200 and `diagnostics` null or empty: the parser found
 * nothing to complain about, because a fragment is syntactically fine. The
 * document that answer carries is then applied, and only /api/validate says
 * what the parse did not: C006 "no workflow found", or the C001/C008 of
 * declarations whose referents live in the fragment's sibling files. Until
 * this check, nothing warned at apply time.
 *
 * The verdict is read from the parse response alone, and only where no other
 * verdict already speaks: a read refusal throws before any apply
 * (refuseReadRefusal), a salvage carries `bindable: false`, and a parse with
 * diagnostics is surfaced by the existing diagnostics panel — all three
 * answer null here, so the detector never doubles a surface that already
 * said it.
 */
export interface FragmentParseWarning {
  kind: "fragment-parse";
  message: string;
}

/** Every declaration list a document can carry, minus `workflows` — the one
 *  whose absence the detector is about. */
function declaresAnything(document: IterDocument): boolean {
  const lists: (readonly unknown[] | undefined)[] = [
    document.imports,
    document.prompts,
    document.schemas,
    document.cursors,
    document.agents,
    document.judges,
    document.routers,
    document.humans,
    document.tools,
    document.computes,
    document.subbots,
    document.fails,
    document.contracts,
    document.mcp_servers,
  ];
  return (
    lists.some((l) => (l ?? []).length > 0) ||
    !!document.vars ||
    !!document.presets ||
    !!document.attachments
  );
}

export function fragmentParseWarning(parsed: {
  document?: IterDocument | null;
  diagnostics?: string[] | null;
  bindable?: boolean;
  read_refusal?: ReadRefusal;
}): FragmentParseWarning | null {
  // The verdicts that already speak, in the order an applying site meets
  // them: the refusal throws, the salvage flags, the diagnostics render.
  if (parsed.read_refusal) return null;
  if (parsed.bindable === false) return null;
  if ((parsed.diagnostics ?? []).length > 0) return null;
  const document = parsed.document;
  if (!document) return null;
  if ((document.workflows ?? []).length > 0) return null;
  return {
    kind: "fragment-parse",
    message: declaresAnything(document)
      ? "Applied, but this text declares no workflow — it reads as a fragment of a bot in several files, not a standalone program. Validation will refuse the document (no workflow) until one is declared."
      : "Applied, but this text declares nothing at all — an empty program that validation will refuse (no workflow) until a workflow is declared.",
  };
}
