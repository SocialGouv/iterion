import { useQuery } from "@tanstack/react-query";

import {
  lookupFileChangeProposal,
  type FileChangeProposalLookup,
} from "@/api/runs/artifacts";

const EMPTY: FileChangeProposalLookup = {
  changes: [],
  sessionId: null,
  revision: null,
  intent: "none",
};

export function useFileChangeProposal(
  runId: string | null,
  revision: number,
): FileChangeProposalLookup {
  const query = useQuery({
    queryKey: ["assistant-file-change", runId ?? "", revision],
    queryFn: ({ signal }) =>
      lookupFileChangeProposal(runId as string, { signal }),
    enabled: !!runId,
    staleTime: 60_000,
    retry: false,
  });
  return query.data ?? EMPTY;
}
