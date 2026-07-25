// Extracted from RunHeader.tsx to keep that file focused.
// Delivery summary row (live URL, running image, source commit, health,
// traceability verdict) shown under the main RunHeader bar when the run
// reported a deployment.
//
// THE INVARIANT: a URL is never presented as a delivery without the
// traceability verdict beside it. An app served from a ConfigMap on a
// stock base image answers 200 and reports every liveness field
// honestly while nothing was pushed and nothing is reproducible — a
// "live ✅" row alone would render that as a success.

import { ExternalLinkIcon } from "@radix-ui/react-icons";

import type { DeploymentReport } from "@/api/runs";
import { Tooltip } from "@/components/ui";
import { useCopyTimer } from "@/hooks/useCopyTimer";
import { useUIStore } from "@/store/ui";

import {
  TRACE_CHIP,
  TRACE_TOOLTIP,
  traceabilityState,
  untraceableReasons,
} from "./deploymentTraceability";

export default function DeploymentRow({
  deployment,
}: {
  deployment: DeploymentReport;
}) {
  const { copied, trigger: triggerCopied } = useCopyTimer<string>(1500);
  const state = traceabilityState(deployment.trace);
  const chip = TRACE_CHIP[state];
  const reasons =
    state === "untraceable" && deployment.trace
      ? untraceableReasons(deployment.trace)
      : [];
  const shortCommit = (deployment.commit ?? "").slice(0, 7);

  const copy = async (text: string, key: string) => {
    try {
      await navigator.clipboard.writeText(text);
      triggerCopied(key);
    } catch {
      // navigator.clipboard is unavailable in insecure contexts (e.g.
      // dev served over plain http without the bypass flag). Surface
      // the failure rather than swallow it so the operator knows the
      // value didn't reach their paste buffer.
      useUIStore.getState().addToast("Copy unavailable in this context", "warning");
    }
  };

  return (
    <div
      className="shrink-0 px-4 py-1.5 bg-surface-2/40 border-b border-border-default text-micro"
      aria-label="Deployment"
    >
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-fg-muted">
          {deployment.deployed ? "deployed" : "deploy"}
        </span>
        {deployment.url ? (
          <a
            href={deployment.url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 font-medium text-accent hover:underline focus-visible:ring-1 focus-visible:ring-accent rounded truncate max-w-md"
            title={`Open ${deployment.url}`}
          >
            <span className="truncate">{deployment.url}</span>
            <ExternalLinkIcon className="w-3 h-3 shrink-0" />
          </a>
        ) : (
          // A deploy that claims to have applied but names no URL is an
          // anomaly worth flagging; on an outright failure the "not
          // deployed" chip below already says it.
          deployment.deployed && (
            <span className="text-fg-subtle">no URL reported</span>
          )
        )}
        {deployment.deployed ? (
          deployment.healthy ? (
            <Tooltip content="The deploying node reported the URL answering healthily.">
              <span className="px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
                healthy
              </span>
            </Tooltip>
          ) : (
            <Tooltip content="The deploy applied, but the URL did not answer healthily.">
              <span className="px-1.5 py-0.5 rounded bg-warning-soft text-warning-fg">
                not healthy
              </span>
            </Tooltip>
          )
        ) : (
          <Tooltip content="The deploy did not apply. Nothing was delivered.">
            <span className="px-1.5 py-0.5 rounded bg-danger-soft text-danger-fg">
              not deployed
            </span>
          </Tooltip>
        )}
        {/* The verdict exists to qualify a URL. With no URL there is no
            delivery claim to qualify, and the chip would read as an
            extra problem on top of an already-reported failure. */}
        {deployment.url && (
          <Tooltip
            content={
              <span>
                {TRACE_TOOLTIP[state]}
                {deployment.trace?.log && (
                  <span className="mt-1 block font-mono text-fg-muted">
                    {deployment.trace.log}
                  </span>
                )}
              </span>
            }
          >
            <span className={`px-1.5 py-0.5 rounded ${chip.className}`}>
              {chip.label}
              {reasons.length > 0 && (
                <span className="ml-1 font-normal opacity-90">
                  — {reasons.join(" · ")}
                </span>
              )}
            </span>
          </Tooltip>
        )}
      </div>
      {(deployment.image_ref || shortCommit || deployment.notes) && (
        <div className="mt-0.5 flex items-center gap-3 flex-wrap text-fg-subtle">
          {deployment.image_ref && (
            <span className="inline-flex items-center gap-1">
              <span className="text-fg-muted">image</span>
              <button
                type="button"
                className="font-mono text-fg-default hover:text-info focus-visible:ring-1 focus-visible:ring-accent rounded truncate max-w-md"
                onClick={() => void copy(deployment.image_ref ?? "", "image")}
                title="Copy the image reference"
              >
                {deployment.image_ref}
                {copied === "image" && (
                  <span className="ml-1 text-fg-subtle">copied</span>
                )}
              </button>
            </span>
          )}
          {shortCommit && (
            <span className="inline-flex items-center gap-1">
              <span className="text-fg-muted">commit</span>
              <button
                type="button"
                className="font-mono text-fg-default hover:text-info focus-visible:ring-1 focus-visible:ring-accent rounded"
                onClick={() => void copy(deployment.commit ?? "", "commit")}
                title="Copy the full SHA of the commit this delivery is anchored to"
              >
                {shortCommit}
                {copied === "commit" && (
                  <span className="ml-1 text-fg-subtle">copied</span>
                )}
              </button>
            </span>
          )}
          {deployment.notes && (
            <span className="truncate max-w-xl" title={deployment.notes}>
              {deployment.notes}
            </span>
          )}
        </div>
      )}
    </div>
  );
}
