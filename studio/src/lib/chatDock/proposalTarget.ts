// Which file of a bot in several files the assistant's proposal applies to
// (#1647, the #1227 tail).
//
// The doctrine (pkg/bundle/chat.go) is that intent grants no authority, and
// `editor_file` is intent's spelling for this surface: a HINT, model-reported,
// which can only ASK. The write target is the Source view picker's selection
// — the file the author is looking at, a fact of the studio, never of the
// model — because a proposal carrying main.bot's text while naming
// lib/nodes.bot would replace the wrong file and nothing downstream could
// tell: the program did change, so sameProgram does not fire, and the
// offer's revision guard is about the store's _generation, not the target
// file. A hint that disagrees with the picker therefore disables Apply until
// the author clicks to switch; it never redirects the write.

import type { UnitInfo } from "@/api/types";
import { SOURCE_MERGED } from "@/store/document";

export type ProposalTarget = { rel: string } | { reason: string };

export function resolveProposalTarget(
  unit: UnitInfo,
  picked: string | null,
  hint: string | null,
): ProposalTarget {
  const files = unit.files.map((f) => f.rel);
  // A hint naming a file the bot does not HAVE is model noise the offer
  // says aloud rather than acting on or silently dropping.
  if (hint && !files.includes(hint)) {
    return {
      reason: `The proposal names ${hint}, which is not one of this bot's files.`,
    };
  }
  // The picker's selection, defaulting to the main exactly as the picker
  // does before it first renders (and a selection whose file left the unit
  // falls back the same way). The merged entry is read-only: one text
  // cannot be split back into the files it came from, so it targets nothing.
  if (picked === SOURCE_MERGED) {
    return {
      reason:
        "The Source view is on the merged program, which no proposal can be split back into — pick the file this proposal replaces there.",
    };
  }
  const target = picked && files.includes(picked) ? picked : unit.main;
  if (hint && hint !== target) {
    return {
      reason: `This proposal is for ${hint}, but the Source view's picker is on ${target}. Switch the picker to ${hint} to apply it.`,
    };
  }
  return { rel: target };
}
