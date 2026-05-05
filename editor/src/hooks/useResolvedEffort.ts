import { useEffect, useState } from "react";
import { fetchResolvedEffort } from "@/api/client";

// Module-level cache keyed by the literal. Resolution depends on the
// iterion process env, which doesn't change during a session, so a
// single fetch per distinct literal is enough.
const cache = new Map<string, string>();
const inflight = new Map<string, Promise<string>>();

// useResolvedEffort returns the env-substituted value for a
// reasoning_effort literal. Pass undefined / a non-env-substituted
// literal and the hook is a no-op (returns the literal unchanged).
//
// Designed for the editor canvas where we want to show "max" in the
// EffortBar instead of the raw "${VIBE_EFFORT:-max}" the author wrote.
export function useResolvedEffort(literal: string | undefined): string | undefined {
  const needsResolve = !!literal && literal.includes("$");
  const [resolved, setResolved] = useState<string | undefined>(() => {
    if (!literal) return undefined;
    if (!needsResolve) return literal;
    return cache.get(literal);
  });

  useEffect(() => {
    if (!literal) {
      setResolved(undefined);
      return;
    }
    if (!needsResolve) {
      setResolved(literal);
      return;
    }
    const cached = cache.get(literal);
    if (cached !== undefined) {
      setResolved(cached);
      return;
    }
    let cancelled = false;
    let promise = inflight.get(literal);
    if (!promise) {
      promise = fetchResolvedEffort(literal).then((r) => {
        cache.set(literal, r);
        return r;
      });
      inflight.set(literal, promise);
      promise.finally(() => inflight.delete(literal));
    }
    promise
      .then((r) => {
        if (!cancelled) setResolved(r);
      })
      .catch(() => {
        if (!cancelled) setResolved(undefined);
      });
    return () => {
      cancelled = true;
    };
  }, [literal, needsResolve]);

  return resolved;
}
