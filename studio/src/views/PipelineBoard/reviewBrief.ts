import type { PipelineBoardReviewBrief } from "@/api/pipelineBoards";

export interface ReviewBrief {
  title: string;
  body: string;
  wordCount: number;
  french: boolean;
}

export type AIReviewContent =
  | {
      kind: "brief";
      points: readonly string[];
    }
  | {
      kind: "legacy";
      points: readonly [string];
    };

// Parse presentation metadata without rewriting or summarizing the authored
// instructions. Concise review points must come from an AI-authored payload.
export function buildReviewBrief(
  instructions: string,
  languageHint = "",
): ReviewBrief {
  const source = instructions.trim();
  const heading = source.match(/^\s*#{1,6}\s+([^\n]+)\s*(?:\n|$)/);
  const rawTitle = heading?.[1]?.trim() ?? "";
  const unheaded = heading ? source.slice(heading[0].length).trim() : source;
  // Rendered DSL prompts may lose the blank line after a compact file list.
  // Markdown then swallows the following instruction into the final bullet.
  // Restore that structural boundary without rewriting the authored prose.
  const body = unheaded.replace(
    /(^\s*-\s+`[^`\n]+`[^\n]*\n)(?=[^\s#*+-])/gm,
    "$1\n",
  );
  const wordCount = body.split(/\s+/).filter(Boolean).length;
  const french = isFrenchReview(`${source}\n${languageHint}`);

  return {
    title: cleanReviewTitle(rawTitle) || (french ? "Votre validation" : "Your review"),
    body,
    wordCount,
    french,
  };
}

// Prefer the typed, top-level API contract. Older workflows already expose a
// genuine AI review in questions.ai_review_detail; preserve that text as one
// indivisible point instead of pretending the client generated a summary.
export function aiReviewContent(
  reviewBrief: PipelineBoardReviewBrief | undefined,
  questions: Record<string, unknown> | null | undefined,
): AIReviewContent | null {
  if (reviewBrief) return { kind: "brief", points: reviewBrief.points };
  const legacy = questions?.ai_review_detail;
  if (typeof legacy !== "string" || !legacy.trim()) return null;
  return { kind: "legacy", points: [legacy.trim()] };
}

function isFrenchReview(source: string): boolean {
  if (/[àâçéèêëîïôûùüÿœ]/i.test(source)) return true;
  const frenchSignals =
    source.match(
      /\b(?:approuvez|consultez|convient|corriger|depuis|revue|refus|uniquement|verifiez|choisissez|indiquez|fichiers)\b/gi,
    )?.length ?? 0;
  const englishSignals =
    source.match(
      /\b(?:approve|check|choose|files|please|reject|review|should|verify|what|which)\b/gi,
    )?.length ?? 0;
  return frenchSignals >= 2 && frenchSignals > englishSignals;
}

function cleanReviewTitle(title: string): string {
  // Machine correlation IDs are useful in technical details, not in the
  // human-facing heading. Keep ordinary em-dash subtitles intact.
  return title
    .replace(/\s+[—–-]\s+[a-z0-9]+(?:-[a-z0-9]+){2,}\s*$/i, "")
    .trim();
}
