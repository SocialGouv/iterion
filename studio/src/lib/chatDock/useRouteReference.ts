// The implicit half of the dock's context: what is visible right now.
// Whether an empty conversation accepts that candidate is conversation
// state, owned by AssistantProvider; route state must not leak a dismissal
// from one conversation into another.

import { useMemo } from "react";
import { useLocation, useSearch } from "wouter";

import {
  pageContextSnapshot,
  useRegisteredAssistantPageContext,
  type AssistantPageContextSnapshot,
} from "./pageContext";
import { referenceForRoute, type TypedReference } from "./routeReference";

export interface RouteReferenceState {
  reference: TypedReference | null;
  // Structured description of the visible page. It has an automatic route
  // floor and merges any state registered by the current view.
  page: AssistantPageContextSnapshot | null;
}

export function useRouteReference(): RouteReferenceState {
  const [location] = useLocation();
  const search = useSearch();
  const contribution = useRegisteredAssistantPageContext();

  const routeReference = useMemo(
    () => referenceForRoute(location, search),
    [location, search],
  );
  const reference = contribution?.reference ?? routeReference;
  const page = useMemo(
    () => pageContextSnapshot(location, reference, contribution),
    [location, reference, contribution],
  );

  return {
    reference,
    page,
  };
}
