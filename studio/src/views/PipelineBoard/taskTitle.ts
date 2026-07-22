export const PIPELINE_TASK_TITLE_MAX_LENGTH = 80;

/**
 * Keep task titles useful as labels. Primary inputs may be complete Markdown
 * briefs; the brief remains in bot_args while the title becomes a compact,
 * single-line preview.
 */
export function compactPipelineTaskTitle(value: string): string {
  const normalized =
    value
      .split(/\r?\n/u)
      .map((line) => line.trim())
      .find(Boolean)
      ?.replace(/\s+/gu, " ") ?? "";
  const characters = Array.from(normalized);
  if (characters.length <= PIPELINE_TASK_TITLE_MAX_LENGTH) return normalized;

  return `${characters
    .slice(0, PIPELINE_TASK_TITLE_MAX_LENGTH - 1)
    .join("")
    .trimEnd()}…`;
}

export function derivePipelineTaskTitle(
  primaryValues: string[],
  fallback: string,
): string {
  const values = primaryValues
    .map(compactPipelineTaskTitle)
    .filter(Boolean);

  return compactPipelineTaskTitle(
    values.length > 0 ? values.join(" · ") : fallback,
  );
}
