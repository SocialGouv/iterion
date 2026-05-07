import { useCallback, useEffect, useMemo, useState } from "react";

import { useRunStore, type BrowserScreenshot } from "@/store/run";
import BrowserLivePane from "./BrowserLivePane";

interface BrowserPaneProps {
  runId: string;
  // When non-null, the run console's scrubber is parked at this seq.
  // The pane swaps the live iframe for the most-recent stored
  // screenshot whose seq <= scrubSeq, so users can rewind through
  // the run's visual timeline. Live iframe returns when scrubSeq
  // becomes null again.
  scrubSeq?: number | null;
}

// pickScreenshotAt returns the latest screenshot with seq <= target,
// or null if none. The list is sorted ascending so binary search
// would be fine; the linear scan is simpler and the list is small
// (a screenshot per browser action, capped at the event window).
function pickScreenshotAt(
  list: BrowserScreenshot[],
  target: number,
): BrowserScreenshot | null {
  let best: BrowserScreenshot | null = null;
  for (const s of list) {
    if (s.seq <= target) {
      best = s;
    } else {
      break;
    }
  }
  return best;
}

// BrowserPane renders a URL inside an iframe, picking the source URL
// from the zustand store's browser state. Two flavours of URL today
// (PR 1): workflow-emitted via `preview_url_available` events, and
// manual URLs typed into the URL bar. Internal-scope URLs are routed
// through the backend preview proxy to strip frame-blocking headers;
// external-scope URLs load directly (and degrade to "open in new tab"
// when the target site refuses framing).
//
// PR 2 will layer a time-travel mode (screenshot artefact for the
// current scrub seq) and PR 3 will add a live CDP screencast mode.
export default function BrowserPane({ runId, scrubSeq = null }: BrowserPaneProps) {
  const browser = useRunStore((s) => s.browser);
  const setManualPreviewUrl = useRunStore((s) => s.setManualPreviewUrl);
  const setLiveSession = useRunStore((s) => s.setLiveSession);

  const [draftUrl, setDraftUrl] = useState<string>(browser.currentUrl ?? "");
  const [attachBusy, setAttachBusy] = useState<boolean>(false);
  const [attachError, setAttachError] = useState<string | null>(null);

  const handleAttach = useCallback(async () => {
    setAttachBusy(true);
    setAttachError(null);
    try {
      const res = await fetch(
        `/api/runs/${encodeURIComponent(runId)}/browser/attach`,
        { method: "POST" },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(`HTTP ${res.status}: ${text}`);
      }
      const body = (await res.json()) as { session_id: string };
      setLiveSession({ sessionId: body.session_id, startedAt: new Date().toISOString() });
    } catch (err) {
      setAttachError(err instanceof Error ? err.message : String(err));
    } finally {
      setAttachBusy(false);
    }
  }, [runId, setLiveSession]);

  const handleDetach = useCallback(() => {
    setLiveSession(null);
  }, [setLiveSession]);

  const scrubbedShot = useMemo(() => {
    if (scrubSeq == null) return null;
    return pickScreenshotAt(browser.screenshots, scrubSeq);
  }, [scrubSeq, browser.screenshots]);
  const isScrubbing = scrubSeq != null;

  // Live Chromium session takes priority over the iframe viewer when
  // not scrubbing: it's the strongest UX signal that the workflow is
  // actively driving a browser. The viewer mode remains the fallback
  // for runs that only publish preview URLs.
  const showLive =
    !isScrubbing &&
    browser.liveSession != null &&
    browser.liveSession.sessionId !== "";

  // Re-sync the URL bar when a new workflow event lands while the
  // user hasn't typed anything. We avoid clobbering an in-progress
  // draft by only syncing when the bar matches the prior store URL
  // (or is empty on first mount).
  useEffect(() => {
    setDraftUrl((prev) => {
      if (prev === "" || prev === browser.currentUrl) {
        return browser.currentUrl ?? "";
      }
      return prev;
    });
  }, [browser.currentUrl]);

  const submitDraft = useCallback(() => {
    const trimmed = draftUrl.trim();
    if (trimmed === "") {
      setManualPreviewUrl(null);
      return;
    }
    setManualPreviewUrl(trimmed);
  }, [draftUrl, setManualPreviewUrl]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === "Enter") {
        e.preventDefault();
        submitDraft();
      }
    },
    [submitDraft],
  );

  const iframeSrc = useMemo(() => {
    if (!browser.currentUrl) return null;
    if (browser.scope === "internal") {
      return `/api/runs/${encodeURIComponent(runId)}/preview?target=${encodeURIComponent(
        browser.currentUrl,
      )}`;
    }
    return browser.currentUrl;
  }, [browser.currentUrl, browser.scope, runId]);

  const sourceLabel = useMemo(() => {
    switch (browser.source) {
      case "tool-stdout":
        return "from tool stdout";
      case "manual":
        return "manual";
      case "runtime":
        return "from runtime";
      default:
        return null;
    }
  }, [browser.source]);

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-1">
      <div className="flex items-center gap-2 border-b border-border-default px-3 py-2">
        <input
          type="url"
          inputMode="url"
          spellCheck={false}
          value={draftUrl}
          onChange={(e) => setDraftUrl(e.target.value)}
          onKeyDown={onKeyDown}
          onBlur={submitDraft}
          placeholder="Enter URL or wait for the workflow to publish one"
          className="flex-1 rounded-md border border-border-default bg-surface-0 px-2 py-1 text-sm font-mono outline-none focus:border-accent"
        />
        {browser.currentUrl && (
          <a
            href={browser.currentUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="text-xs text-text-2 underline hover:text-text-1"
            title="Open in a new tab — useful when the target site forbids embedding"
          >
            open ↗
          </a>
        )}
        <button
          type="button"
          onClick={() => {
            setDraftUrl("");
            setManualPreviewUrl(null);
          }}
          className="text-xs text-text-2 hover:text-text-1"
          title="Clear the current preview"
        >
          clear
        </button>
        {showLive ? (
          <button
            type="button"
            onClick={handleDetach}
            className="text-xs text-amber-500 hover:text-amber-400"
            title="Stop the live Chromium session and return to viewer mode"
          >
            stop live
          </button>
        ) : (
          <button
            type="button"
            onClick={handleAttach}
            disabled={attachBusy}
            className="text-xs text-emerald-500 hover:text-emerald-400 disabled:opacity-50"
            title="Spawn a host Chromium and stream it here (requires chromium installed on PATH)"
          >
            {attachBusy ? "attaching…" : "attach live"}
          </button>
        )}
      </div>
      {attachError ? (
        <div className="border-b border-red-700 bg-red-950/40 px-3 py-1 text-[11px] text-red-300">
          {attachError}
        </div>
      ) : null}
      <div className="flex-1 min-h-0 bg-surface-0">
        {showLive && browser.liveSession ? (
          <BrowserLivePane runId={runId} sessionId={browser.liveSession.sessionId} />
        ) : isScrubbing ? (
          scrubbedShot ? (
            <img
              key={scrubbedShot.attachmentName}
              src={`/api/runs/${encodeURIComponent(runId)}/attachments/${encodeURIComponent(scrubbedShot.attachmentName)}`}
              alt={`Screenshot at seq ${scrubbedShot.seq}`}
              className="h-full w-full object-contain"
            />
          ) : (
            <div className="flex h-full items-center justify-center p-6 text-center text-sm text-text-2">
              No screenshot captured before seq {scrubSeq}. The workflow
              had not yet published a frame at this point.
            </div>
          )
        ) : iframeSrc ? (
          <iframe
            key={iframeSrc}
            src={iframeSrc}
            title="Preview"
            className="h-full w-full border-0"
            sandbox="allow-scripts allow-forms allow-same-origin allow-popups"
          />
        ) : (
          <div className="flex h-full items-center justify-center p-6 text-center text-sm text-text-2">
            No preview URL yet. The workflow can publish one with{" "}
            <code className="mx-1 rounded bg-surface-2 px-1 font-mono">
              [iterion] preview_url=&lt;url&gt;
            </code>{" "}
            on a tool node's stdout, or you can type one above.
          </div>
        )}
      </div>
      {showLive ? null : isScrubbing ? (
        <div className="border-t border-border-default px-3 py-1 text-[11px] text-amber-500">
          scrubbed at seq {scrubSeq}
          {scrubbedShot
            ? ` · screenshot from ${scrubbedShot.nodeId ?? "unknown"} (seq ${scrubbedShot.seq})`
            : ""}
        </div>
      ) : sourceLabel ? (
        <div className="border-t border-border-default px-3 py-1 text-[11px] text-text-2">
          {sourceLabel}
          {browser.kind ? ` · ${browser.kind}` : null}
          {browser.scope === "internal" ? " · proxied" : null}
        </div>
      ) : null}
    </div>
  );
}
