// The implicit half of the dock's context: what the operator is looking
// at right now, and whether they still want the assistant to assume it.
//
// Dismissal is keyed on the reference itself, not on a boolean: dismiss
// `run/019f…` and the chip stays gone for that run while you navigate
// away and back, but /board still contributes its own. In-memory by
// design — a dismissal is "not this conversation", not a preference.
//
// It is held by AssistantProvider, not by this hook's own state: the
// dock unmounts on /whats-next, which would otherwise resurrect a
// dismissed chip on the round trip. Local state is the fallback for a
// caller outside the provider.

import { useCallback, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { useAssistantDock } from "@/components/ChatDock/AssistantProvider";

import { referenceForRoute, type TypedReference } from "./routeReference";

export interface RouteReferenceState {
  // What the route points at, before dismissal is applied. Non-null
  // even when dismissed, so the dock can offer to restore it.
  reference: TypedReference | null;
  // What the assistant is actually told. Null when there is nothing to
  // point at OR the operator dismissed it.
  active: TypedReference | null;
  dismissed: boolean;
  dismiss: () => void;
  restore: () => void;
}

// activeReference is the whole dismissal rule, kept pure so it can be
// tested without a router.
export function activeReference(
  reference: TypedReference | null,
  dismissedRef: string | null,
): TypedReference | null {
  if (!reference) return null;
  return reference.ref === dismissedRef ? null : reference;
}

export function useRouteReference(): RouteReferenceState {
  const [location] = useLocation();
  const search = useSearch();
  const dockCtx = useAssistantDock();
  // Both hooks run unconditionally (hook order); the provider's state
  // wins whenever there is one.
  const [localRef, setLocalRef] = useState<string | null>(null);
  const dismissedRef = dockCtx ? dockCtx.dismissedRef : localRef;
  const setDismissedRef = dockCtx ? dockCtx.setDismissedRef : setLocalRef;

  const reference = useMemo(
    () => referenceForRoute(location, search),
    [location, search],
  );
  const active = activeReference(reference, dismissedRef);

  const dismiss = useCallback(() => {
    setDismissedRef(reference?.ref ?? null);
  }, [reference, setDismissedRef]);
  const restore = useCallback(() => setDismissedRef(null), [setDismissedRef]);

  return {
    reference,
    active,
    dismissed: reference !== null && active === null,
    dismiss,
    restore,
  };
}
