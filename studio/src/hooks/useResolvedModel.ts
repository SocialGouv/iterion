import { useQueries, useQuery } from "@tanstack/react-query";

import { fetchResolvedModel } from "@/api/client";

// useResolvedModel returns the env-substituted value for a model
// literal. Pass undefined / a non-env-substituted literal and the
// hook is a no-op (returns the literal unchanged).
//
// Designed for the studio canvas where we want to show "gpt-5.6-sol"
// instead of the raw "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}" the
// author wrote — and to surface a live env override (terra/luna)
// when one is set. Resolution depends on the iterion process env
// which doesn't change during a session, so cache forever once resolved.
export function useResolvedModel(literal: string | undefined): string | undefined {
  const needsResolve = !!literal && literal.includes("$");
  const query = useQuery<string>({
    queryKey: ["resolved-model", literal],
    queryFn: () => fetchResolvedModel(literal!),
    enabled: needsResolve,
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: Number.POSITIVE_INFINITY,
  });
  if (!literal) return undefined;
  if (!needsResolve) return literal;
  return query.data || undefined;
}

// useResolvedModels is the list form for a node's `fallbacks:` models.
// useQueries (not a hook-in-a-loop) so the chain length can vary.
export function useResolvedModels(
  literals: Array<string | undefined>,
): Array<string | undefined> {
  const queries = useQueries({
    queries: literals.map((literal) => ({
      queryKey: ["resolved-model", literal ?? ""],
      queryFn: () => fetchResolvedModel(literal ?? ""),
      enabled: !!literal && literal.includes("$"),
      staleTime: Number.POSITIVE_INFINITY,
      gcTime: Number.POSITIVE_INFINITY,
    })),
  });
  return literals.map((literal, i) => {
    if (!literal) return undefined;
    if (!literal.includes("$")) return literal;
    return queries[i]?.data || undefined;
  });
}
