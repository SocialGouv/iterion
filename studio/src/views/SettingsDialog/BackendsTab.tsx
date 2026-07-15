import { ReloadIcon } from "@radix-ui/react-icons";

import { useBackendDetectStore } from "@/store/backendDetect";
import { BackendBadge } from "@/components/icons/BackendBadge";
import { InlineBanner } from "@/components/ui/InlineBanner";
import type { BackendStatus } from "@/api/backends";
import { desktop, isDesktop } from "@/lib/desktopBridge";

const DOCS_BACKENDS_URL =
  "https://github.com/SocialGouv/iterion/blob/main/docs/backends.md";

function openDocs(e: React.MouseEvent<HTMLAnchorElement>) {
  if (isDesktop()) {
    e.preventDefault();
    void desktop.openExternal(DOCS_BACKENDS_URL);
  }
}

export default function BackendsTab() {
  const report = useBackendDetectStore((s) => s.report);
  const loading = useBackendDetectStore((s) => s.loading);
  const error = useBackendDetectStore((s) => s.error);
  const refresh = useBackendDetectStore((s) => s.refresh);

  const resolved = report?.resolved_default;
  const hasAny = !!resolved;

  return (
    <div className="flex flex-col gap-3 p-4 text-sm">
      <div className="flex items-start justify-between gap-2">
        <div>
          <div className="font-medium text-fg-default">LLM credentials</div>
          <p className="text-xs text-fg-subtle">
            Auto-detected at server start and refreshed on demand. Each agent
            node can override via its <code>backend</code> field.
          </p>
        </div>
        <button
          type="button"
          onClick={() => void refresh(true)}
          disabled={loading}
          className="inline-flex items-center gap-1 text-xs text-fg-subtle hover:text-fg-default disabled:opacity-50"
        >
          <ReloadIcon className={`w-3 h-3 ${loading ? "animate-spin" : ""}`} />
          Refresh
        </button>
      </div>

      {error && (
        <InlineBanner tone="danger" layout="inline" title="Backend detect failed">
          {error}
        </InlineBanner>
      )}

      {report && (
        <>
          <div className="rounded border border-border-default bg-surface-1 p-3">
            <div className="text-xs text-fg-subtle mb-1">Resolved default</div>
            {hasAny ? (
              <div className="flex items-center gap-2">
                <span
                  role="img"
                  aria-label="available"
                  className="inline-block w-2 h-2 rounded-full bg-success"
                />
                <BackendBadge backend="" resolved={resolved!} size={12} showLabel />
              </div>
            ) : (
              <InlineBanner tone="warning" layout="inline">
                No credential detected. The Run button will fail until one of
                the options below is configured.
              </InlineBanner>
            )}
            <div className="text-caption text-fg-subtle mt-2">
              Preference order: {report.preference_order.join(" → ")}
            </div>
          </div>

          <ul className="space-y-2">
            {report.backends.map((b) => (
              <BackendRow key={b.name} status={b} />
            ))}
          </ul>
        </>
      )}

      <div className="pt-1">
        <a
          href={DOCS_BACKENDS_URL}
          target="_blank"
          rel="noopener noreferrer"
          onClick={openDocs}
          className="text-xs text-fg-subtle hover:text-fg-default hover:underline"
        >
          Backends reference ↗
        </a>
      </div>
    </div>
  );
}

function BackendRow({ status }: { status: BackendStatus }) {
  return (
    <li className="rounded border border-border-default bg-surface-1 p-2 flex items-start gap-2">
      <span
        role="img"
        aria-label={status.available ? "available" : "unavailable"}
        className={`mt-1 inline-block w-2 h-2 rounded-full shrink-0 ${
          status.available ? "bg-success" : "bg-fg-subtle/40"
        }`}
      />
      <div className="flex-1 min-w-0 text-xs">
        <div className="flex items-center gap-1">
          <span className="font-medium text-fg-default">{status.name}</span>
          {status.auth !== "none" && (
            <span className="text-fg-subtle">· {status.auth}</span>
          )}
        </div>
        {(status.sources?.length ?? 0) > 0 && (
          <div className="text-fg-subtle">
            {status.sources!.map((src, i) => {
              const overridden = src.includes("(overridden by ");
              return (
                <span key={i} className={overridden ? "line-through opacity-60" : ""}>
                  {i > 0 && <span className="opacity-50">, </span>}
                  <span title={overridden ? src : undefined}>{src}</span>
                </span>
              );
            })}
          </div>
        )}
        {!status.available && (status.hints?.length ?? 0) > 0 && (
          <ul className="text-fg-subtle list-disc list-inside mt-1">
            {status.hints!.map((h, i) => (
              <li key={i}>{h}</li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}
