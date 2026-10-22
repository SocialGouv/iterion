// Deliverables row: the files a run left in its artifact-files area
// (reports, SBOMs, bundles), surfaced in the run summary next to the
// result links so a reader finds "what did it produce" without opening
// the Artifacts drawer. Each entry opens the file's rendered preview on
// the run page (a shareable `?file=` address, carrying the current query
// along) and offers the same download gesture as the files panel —
// desktop save path and downloads history included. The listing is kept
// fresh by the run's own events, so a file that lands mid-run appears
// here while the operator watches another drawer tab.
import { DownloadIcon, FileTextIcon } from "@radix-ui/react-icons";
import { Link, useLocation, useSearch } from "wouter";
import { artifactFileRunPath } from "../artifacts/artifactFileLink";
import {
  useArtifactDownload,
  useArtifactFilesQuery,
  useArtifactFilesRefresh,
} from "../artifacts/useArtifactFiles";

export default function DeliverablesRow({ runId }: { runId: string }) {
  const filesQuery = useArtifactFilesQuery(runId || null);
  useArtifactFilesRefresh(runId || null);
  const search = useSearch();
  const [, navigate] = useLocation();
  const triggerDownload = useArtifactDownload(runId || null, (target) =>
    navigate(artifactFileRunPath(runId, target.path, search)),
  );
  const files = filesQuery.data ?? [];
  if (files.length === 0) return null;
  return (
    <div
      role="group"
      className="shrink-0 px-4 py-2 border-b border-border-default flex flex-wrap items-center gap-x-4 gap-y-1 text-body"
      aria-label="Run deliverables"
    >
      <span className="inline-flex items-center gap-1.5 font-medium text-fg-default shrink-0">
        <FileTextIcon className="w-4 h-4 text-accent-text" aria-hidden="true" />
        Deliverables
      </span>
      {files.map((f) => (
        <span key={f.path} className="inline-flex items-center gap-1.5 min-w-0">
          <Link
            href={artifactFileRunPath(runId, f.path, search)}
            className="font-mono text-accent-text hover:underline truncate focus-visible:ring-1 focus-visible:ring-accent rounded"
            title={`Open ${f.path}`}
          >
            {f.path}
          </Link>
          <button
            type="button"
            onClick={() => triggerDownload({ path: f.path, size: f.size })}
            className="inline-flex text-fg-subtle hover:text-fg-default focus-visible:ring-1 focus-visible:ring-accent rounded"
            aria-label={`Download ${f.path}`}
            title={`Download ${f.path}`}
          >
            <DownloadIcon className="w-3.5 h-3.5" aria-hidden="true" />
          </button>
        </span>
      ))}
    </div>
  );
}
