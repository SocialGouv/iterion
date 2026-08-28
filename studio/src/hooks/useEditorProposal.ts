import { useQuery } from "@tanstack/react-query";

import {
  lookupEditorProposal,
  type EditorProposalLookup,
} from "@/api/runs/artifacts";

const EMPTY: EditorProposalLookup = {
  source: null,
  sessionId: null,
  revision: null,
};

export function editorProposalQueryKey(runId: string, revision: number) {
  return ["editor-proposal", runId, revision] as const;
}

export function useEditorProposal(
  runId: string | null,
  revision: number,
): EditorProposalLookup {
  const query = useQuery({
    queryKey: editorProposalQueryKey(runId ?? "", revision),
    queryFn: ({ signal }) =>
      lookupEditorProposal(runId as string, { signal }),
    enabled: !!runId,
    staleTime: 60_000,
    retry: false,
  });
  return query.data ?? EMPTY;
}

