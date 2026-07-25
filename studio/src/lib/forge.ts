/**
 * Forge-aware terminology. GitLab calls the code-review unit a "merge
 * request"; GitHub, Forgejo/Gitea and every other supported forge call
 * it a "pull request". Default to PR when the provider is unknown so
 * generic UI never shows GitLab-only wording.
 */

export type ForgeProviderLike = string | null | undefined;

export interface ForgeWording {
  /** Short noun: "PR" | "MR". */
  noun: "PR" | "MR";
  /** Long noun: "pull request" | "merge request". */
  long: "pull request" | "merge request";
}

export function forgeLabel(provider: ForgeProviderLike): ForgeWording {
  if ((provider ?? "").toLowerCase() === "gitlab") {
    return { noun: "MR", long: "merge request" };
  }
  return { noun: "PR", long: "pull request" };
}
