// Extensible description of what the operator can currently see.
//
// Route context has a useful automatic floor (route + typed reference), but
// route params cannot describe UI state such as the selected editor node or
// an unsaved buffer. Views contribute that richer state through
// useAssistantPageContext. Contributions are merged so a route shell can name
// the entity while a nested panel names the active section.
/* eslint-disable react-refresh/only-export-components */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

import type { TypedReference } from "./routeReference";

export type PageContextScalar = string | number | boolean | null;
export type PageContextValue =
  | PageContextScalar
  | PageContextValue[]
  | { [key: string]: PageContextValue };

export interface PageContextEntity {
  type: string;
  id: string;
  label?: string;
}

export interface AssistantPageContextContribution {
  title?: string;
  section?: string;
  entity?: PageContextEntity;
  state?: Record<string, PageContextValue>;
}

export interface AssistantPageContextSnapshot
  extends AssistantPageContextContribution {
  // Pathname only. Query strings can contain tokens and other values that a
  // generic mechanism must never forward. A view may expose a safe,
  // meaningful query-derived value explicitly through section/state.
  route: string;
}

interface RegisteredContribution {
  token: symbol;
  value: AssistantPageContextContribution;
}

interface RegistryValue {
  contribution: AssistantPageContextContribution | null;
  register: (
    token: symbol,
    value: AssistantPageContextContribution,
  ) => () => void;
}

const PageContextRegistry = createContext<RegistryValue | null>(null);

export function AssistantPageContextProvider({
  children,
}: {
  children: ReactNode;
}) {
  const [entries, setEntries] = useState<RegisteredContribution[]>([]);

  const register = useCallback(
    (token: symbol, value: AssistantPageContextContribution) => {
      setEntries((current) => {
        const index = current.findIndex((entry) => entry.token === token);
        if (index === -1) return [...current, { token, value }];
        if (current[index]?.value === value) return current;
        const next = [...current];
        next[index] = { token, value };
        return next;
      });
      return () => {
        setEntries((current) => current.filter((entry) => entry.token !== token));
      };
    },
    [],
  );

  const contribution = useMemo(
    () => mergePageContextContributions(entries.map((entry) => entry.value)),
    [entries],
  );
  const value = useMemo<RegistryValue>(
    () => ({ contribution, register }),
    [contribution, register],
  );

  return (
    <PageContextRegistry.Provider value={value}>
      {children}
    </PageContextRegistry.Provider>
  );
}

/**
 * Register the context a visible view adds to the route-level floor.
 *
 * Pass enabled=false for mounted-but-hidden panes (the editor keeps inactive
 * tabs alive). Values should be memoised by the caller when practical; a
 * semantic change is intentionally published immediately so the next message
 * uses what is on screen at send time.
 */
export function useAssistantPageContext(
  value: AssistantPageContextContribution,
  enabled = true,
): void {
  const register = useContext(PageContextRegistry)?.register;
  const token = useRef(Symbol("assistant-page-context"));
  // Callers often assemble a small object inline. Publishing on object
  // identity would make the provider update, re-render the caller, create a
  // fresh object and loop forever. Contributions are JSON-shaped by contract,
  // so semantic equality is the right stability boundary.
  const signature = JSON.stringify(value);
  const stableValue = useMemo(
    () => JSON.parse(signature) as AssistantPageContextContribution,
    [signature],
  );

  useEffect(() => {
    if (!register || !enabled) return;
    return register(token.current, stableValue);
  }, [register, stableValue, enabled]);
}

export function useRegisteredAssistantPageContext(): AssistantPageContextContribution | null {
  return useContext(PageContextRegistry)?.contribution ?? null;
}

export function mergePageContextContributions(
  values: readonly AssistantPageContextContribution[],
): AssistantPageContextContribution | null {
  if (values.length === 0) return null;
  const merged: AssistantPageContextContribution = {};
  let state: Record<string, PageContextValue> | undefined;
  for (const value of values) {
    if (value.title !== undefined) merged.title = value.title;
    if (value.section !== undefined) merged.section = value.section;
    if (value.entity !== undefined) merged.entity = value.entity;
    if (value.state !== undefined) state = { ...state, ...value.state };
  }
  if (state) merged.state = state;
  return merged;
}

export function pageContextSnapshot(
  route: string,
  reference: TypedReference | null,
  contribution: AssistantPageContextContribution | null,
): AssistantPageContextSnapshot | null {
  // /whats-next intentionally has no page context: it already is the same
  // assistant conversation and the dock stands down there.
  if (!reference && !contribution) return null;

  const entity = contribution?.entity ?? entityFromReference(reference);
  return {
    route,
    title: contribution?.title ?? reference?.label ?? "Current page",
    ...(contribution?.section ? { section: contribution.section } : {}),
    ...(entity ? { entity } : {}),
    ...(contribution?.state ? { state: contribution.state } : {}),
  };
}

function entityFromReference(
  reference: TypedReference | null,
): PageContextEntity | undefined {
  if (!reference || reference.kind === "view") return undefined;
  const slash = reference.ref.indexOf("/");
  const id = slash === -1 ? "" : reference.ref.slice(slash + 1);
  if (!id) return undefined;
  return { type: reference.kind, id, label: reference.label };
}
