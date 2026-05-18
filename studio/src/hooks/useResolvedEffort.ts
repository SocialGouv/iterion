import { useQuery } from "@tanstack/react-query";

import { fetchResolvedEffort } from "@/api/client";

// useResolvedEffort returns the env-substituted value for a
// reasoning_effort literal. Pass undefined / a non-env-substituted
// literal and the hook is a no-op (returns the literal unchanged).
//
// Designed for the studio canvas where we want to show "max" in the
// EffortBar instead of the raw "${VIBE_EFFORT:-max}" the author wrote.
// Resolution depends on the iterion process env which doesn't change
// during a session, so cache forever once resolved.
export function useResolvedEffort(literal: string | undefined): string | undefined {
  const needsResolve = !!literal && literal.includes("$");
  const query = useQuery<string>({
    queryKey: ["resolved-effort", literal],
    queryFn: () => fetchResolvedEffort(literal!),
    enabled: needsResolve,
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: Number.POSITIVE_INFINITY,
  });
  if (!literal) return undefined;
  if (!needsResolve) return literal;
  return query.data;
}
