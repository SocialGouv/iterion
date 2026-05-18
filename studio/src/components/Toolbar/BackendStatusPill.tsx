import { ReloadIcon } from "@radix-ui/react-icons";

import { useBackendDetectStore } from "@/store/backendDetect";
import { BackendBadge } from "@/components/icons/BackendBadge";
import { Popover } from "@/components/ui/Popover";
import type { BackendStatus } from "@/api/backends";
import { desktop, isDesktop } from "@/lib/desktopBridge";

const DOCS_BACKENDS_URL =
  "https://github.com/SocialGouv/iterion/blob/main/docs/backends.md";

function openDocs(e: React.MouseEvent<HTMLAnchorElement>) {
  // In the Wails-hosted desktop app, the webview blocks navigation to
  // external origins so a plain target="_blank" silently does nothing.
  // Route through the OpenExternal Wails binding when available.
  if (isDesktop()) {
    e.preventDefault();
    void desktop.openExternal(DOCS_BACKENDS_URL);
  }
}

export default function BackendStatusPill() {
  const report = useBackendDetectStore((s) => s.report);
  const loading = useBackendDetectStore((s) => s.loading);
  const error = useBackendDetectStore((s) => s.error);
  const refresh = useBackendDetectStore((s) => s.refresh);

  if (loading && !report) {
    return (
      <span className="text-[10px] text-fg-subtle px-2 py-1" title="Detecting LLM backends...">
        …
      </span>
    );
  }

  if (error) {
    return (
      <button
        type="button"
        className="text-[10px] text-error px-2 py-1"
        title={`Backend detect failed: ${error}`}
        onClick={() => void refresh()}
      >
        ⚠ creds
      </button>
    );
  }

  if (!report) return null;

  const resolved = report.resolved_default;
  const hasAny = !!resolved;

  const summary = report.backends
    .map((b) => `${b.available ? "✓" : "·"} ${b.name}${b.auth !== "none" ? ` (${b.auth})` : ""}`)
    .join("\n");
  const tooltip = `Preference: ${report.preference_order.join(" → ")}\n${summary}`;

  return (
    <Popover
      side="bottom"
      align="start"
      contentClassName="min-w-[280px] p-3 text-xs"
      trigger={
        <button
          type="button"
          className={`inline-flex items-center gap-1 px-2 py-0.5 rounded text-[10px] border ${
            hasAny
              ? "border-success/40 text-success bg-success/5"
              : "border-error/50 text-error bg-error/5"
          }`}
          title={tooltip}
        >
          <span
            aria-hidden
            className={`inline-block w-1.5 h-1.5 rounded-full ${
              hasAny ? "bg-success" : "bg-error"
            }`}
          />
          {hasAny ? (
            <BackendBadge backend="" resolved={resolved} size={9} showLabel />
          ) : (
            <span>no creds</span>
          )}
        </button>
      }
    >
      <div className="font-semibold mb-1">LLM credentials</div>
      {hasAny ? (
        <p className="text-fg-subtle mb-2">
          Auto-resolved: <span className="text-fg-default">{resolved}</span>. Edit
          an agent's <code>backend</code> field to override per-node.
        </p>
      ) : (
        <p className="text-error mb-2">
          No credential detected. The Run button will fail until you
          configure one of the options below.
        </p>
      )}
      <ul className="space-y-1">
        {report.backends.map((b) => (
          <BackendRow key={b.name} status={b} />
        ))}
      </ul>
      <div className="mt-3 flex items-center justify-between gap-2 text-[10px]">
        <button
          type="button"
          className="inline-flex items-center gap-1 text-fg-subtle hover:text-fg-default disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:text-fg-subtle"
          onClick={() => void refresh(true)}
          disabled={loading}
        >
          <ReloadIcon className={`w-2.5 h-2.5 ${loading ? "animate-spin" : ""}`} />
          Refresh
        </button>
        <a
          href={DOCS_BACKENDS_URL}
          target="_blank"
          rel="noopener noreferrer"
          onClick={openDocs}
          className="text-fg-subtle hover:text-fg-default hover:underline cursor-pointer"
        >
          Backends reference ↗
        </a>
      </div>
    </Popover>
  );
}

function BackendRow({ status }: { status: BackendStatus }) {
  return (
    <li className="flex items-start gap-2">
      <span
        className={`mt-0.5 inline-block w-1.5 h-1.5 rounded-full ${
          status.available ? "bg-success" : "bg-fg-subtle/40"
        }`}
      />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-1">
          <span className="font-medium">{status.name}</span>
          {status.auth !== "none" && (
            <span className="text-fg-subtle text-[10px]">· {status.auth}</span>
          )}
        </div>
        {(status.sources?.length ?? 0) > 0 && (
          <div className="text-fg-subtle text-[10px]">
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
          <ul className="text-fg-subtle text-[10px] list-disc list-inside">
            {status.hints!.map((h, i) => (
              <li key={i}>{h}</li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}
