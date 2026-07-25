import type { PipelineBoardReviewBrief } from "@/api/pipelineBoards";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";

import { aiReviewContent, buildReviewBrief } from "./reviewBrief";

export function ReviewInstructions({
  instructions,
  reviewBrief,
  questions,
}: {
  instructions: string;
  reviewBrief?: PipelineBoardReviewBrief;
  questions?: Record<string, unknown>;
}) {
  const aiContent = aiReviewContent(reviewBrief, questions);
  const brief = buildReviewBrief(instructions, aiContent?.points.join("\n"));

  if (!aiContent) {
    return (
      <section
        aria-label="Review question"
        className="space-y-4 rounded-lg border border-border-default bg-surface-1 p-4"
      >
        <h4 className="text-display font-semibold text-fg-default">
          {brief.title}
        </h4>
        <p className="rounded-md border border-border-subtle bg-surface-0 px-3 py-2 text-title leading-relaxed text-fg-muted">
          {brief.french
            ? "Aucun résumé IA n’a été fourni pour cette revue."
            : "No AI summary was provided for this review."}
        </p>
        <div className="text-fg-default">
          <MarkdownText value={brief.body} size="lg" />
        </div>
      </section>
    );
  }

  return (
    <section
      aria-label="Review question"
      className="space-y-4 rounded-lg border border-border-default bg-surface-1 p-4"
    >
      <div className="space-y-2">
        <p className="text-body font-medium uppercase tracking-wide text-warning-fg">
          {aiContent.kind === "legacy"
            ? brief.french
              ? "Synthèse de la revue IA"
              : "AI review summary"
            : brief.french
              ? "Consignes de la revue IA"
              : "AI review instructions"}
        </p>
        <h4 className="text-display font-semibold text-fg-default">{brief.title}</h4>
      </div>

      {aiContent.kind === "brief" ? (
        <ol className="space-y-2 text-title leading-relaxed text-fg-default">
          {aiContent.points.map((point, index) => (
            <li key={`${index}:${point}`} className="flex gap-3">
              <span
                aria-hidden
                className="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-accent-soft text-body font-semibold text-accent-text"
              >
                {index + 1}
              </span>
              <span className="whitespace-pre-wrap">{point}</span>
            </li>
          ))}
        </ol>
      ) : (
        <p className="whitespace-pre-wrap text-title leading-relaxed text-fg-default">
          {aiContent.points[0]}
        </p>
      )}

      {brief.body && (
        <details className="rounded-md border border-border-subtle bg-surface-0">
          <summary className="cursor-pointer px-3 py-2 text-body font-medium text-fg-muted hover:text-fg-default">
            {brief.french ? "Afficher les critères détaillés" : "Show detailed criteria"}
            <span className="ml-1 font-normal text-fg-subtle">
              · {brief.wordCount} {brief.french ? "mots" : "words"}
            </span>
          </summary>
          <div className="border-t border-border-subtle px-3 py-3">
            <MarkdownText value={brief.body} size="lg" />
          </div>
        </details>
      )}
    </section>
  );
}

export default ReviewInstructions;
