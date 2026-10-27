import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Tooltip } from "@/components/ui";
import { useRunStore, type BrowserScreenshot } from "@/store/run";
import BrowserLivePane from "./BrowserLivePane";

export type BrowserDock = "bottom" | "right";

interface BrowserPaneProps {
  runId: string;
  // When non-null, the run console's scrubber is parked at this seq.
  // The pane swaps the live iframe for the most-recent stored
  // screenshot whose seq <= scrubSeq, so users can rewind through
  // the run's visual timeline. Live iframe returns when scrubSeq
  // becomes null again.
  scrubSeq?: number | null;
  // dock controls where this pane is rendered. The button in the
  // header just toggles; the parent decides where to mount the
  // component. Optional (defaults to "bottom") so external callers
  // that don't yet support docking keep working.
  dock?: BrowserDock;
  onDockChange?: (next: BrowserDock) => void;
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
// from the zustand store's browser state. URLs come from two places:
// workflow-emitted `preview_url_available` events, and manual URLs
// typed into the URL bar. Internal-scope URLs are routed through the
// backend preview proxy to strip frame-blocking headers;
// external-scope URLs load directly (and degrade to "open in new tab"
// when the target site refuses framing).
//
// The pane has three modes: live CDP screencast (when liveSession is
// set), time-travel screenshot (when the run-console scrubber is
// parked), or viewer iframe (the default). Mode priority is
// live > time-travel > viewer.
export default function BrowserPane({
  runId,
  scrubSeq = null,
  dock = "bottom",
  onDockChange,
}: BrowserPaneProps) {
  const browser = useRunStore((s) => s.browser);
  const setManualPreviewUrl = useRunStore((s) => s.setManualPreviewUrl);
  const setLiveSession = useRunStore((s) => s.setLiveSession);

  const [draftUrl, setDraftUrl] = useState<string>(browser.currentUrl ?? "");
  const [attachBusy, setAttachBusy] = useState<boolean>(false);
  const [attachError, setAttachError] = useState<string | null>(null);
  // Track the previous store-driven URL so the resync effect can
  // compare against THAT, not against the new value that just
  // arrived. Without this, a workflow event landing the same render
  // as user typing would clobber the keystroke (prev === new is true
  // even though the user actually changed prev mid-render).
  const lastStoreUrlRef = useRef<string | undefined>(browser.currentUrl);
  // The live attach POSTs /browser/attach — a member action on the run
  // ladder (ADR-103). A viewer's affordance is simply absent instead of
  // error-on-click, the same rule the file editor's gate follows.
  const viewer = useRunStore((s) => s.viewer);
  const canAttach = viewer ? viewer.role !== "viewer" : true;
  // Cancel in-flight attach + skip stale setState on unmount.
  const attachAbortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    return () => {
      attachAbortRef.current?.abort();
    };
  }, []);

  const handleAttach = useCallback(async () => {
    attachAbortRef.current?.abort();
    const controller = new AbortController();
    attachAbortRef.current = controller;
    setAttachBusy(true);
    setAttachError(null);
    try {
      const res = await fetch(
        `/api/runs/${encodeURIComponent(runId)}/browser/attach`,
        { method: "POST", credentials: "include", signal: controller.signal },
      );
      if (!res.ok) {
        const text = await res.text();
        throw new Error(`HTTP ${res.status}: ${text}`);
      }
      const body = (await res.json()) as { session_id: string };
      if (controller.signal.aborted) return;
      setLiveSession({ sessionId: body.session_id, startedAt: new Date().toISOString() });
    } catch (err) {
      if (controller.signal.aborted) return;
      setAttachError(errorMessage(err));
    } finally {
      if (!controller.signal.aborted) setAttachBusy(false);
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
  // user hasn't typed anything. The previous-URL ref lets us compare
  // the draft against what the store used to say, not the new value
  // that just arrived — a keystroke landing in the same render as a
  // workflow URL update used to be silently rewritten.
  useEffect(() => {
    const previousStoreUrl = lastStoreUrlRef.current;
    lastStoreUrlRef.current = browser.currentUrl;
    setDraftUrl((prev) => {
      if (prev === "" || prev === previousStoreUrl) {
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
          aria-label="Browser URL"
          className="flex-1 rounded-md border border-border-control bg-surface-0 px-2 py-1 text-sm font-mono outline-none focus:border-accent"
        />
        {browser.currentUrl && (
          <Tooltip content="Open in a new tab — useful when the target site forbids embedding">
            <a
              href={browser.currentUrl}
              target="_blank"
              rel="noopener noreferrer"
              aria-label={`Open ${browser.currentUrl} in a new tab`}
              className="text-xs text-fg-muted underline hover:text-fg-default"
            >
              open ↗
            </a>
          </Tooltip>
        )}
        <Tooltip content="Clear the current preview">
          <button
            type="button"
            onClick={() => {
              setDraftUrl("");
              setManualPreviewUrl(null);
            }}
            className="text-xs text-fg-muted hover:text-fg-default"
          >
            clear
          </button>
        </Tooltip>
        {showLive ? (
          <Tooltip content="Stop the live Chromium session and return to viewer mode">
            <button
              type="button"
              onClick={handleDetach}
              className="text-xs text-warning hover:text-warning-fg"
            >
              stop live
            </button>
          </Tooltip>
        ) : (
          canAttach && (
            <Tooltip content="Spawn a host Chromium and stream it here (requires chromium installed on PATH)">
              <button
                type="button"
                onClick={handleAttach}
                disabled={attachBusy}
                className="text-xs text-success hover:text-success-fg disabled:opacity-50"
              >
                {attachBusy ? "attaching…" : "attach live"}
              </button>
            </Tooltip>
          )
        )}
        {onDockChange ? (
          <Tooltip
            content={
              dock === "bottom"
                ? "Dock the browser pane on the right side"
                : "Dock the browser pane at the bottom"
            }
          >
            <button
              type="button"
              onClick={() => onDockChange(dock === "bottom" ? "right" : "bottom")}
              className="text-xs text-fg-muted hover:text-fg-default"
            >
              {dock === "bottom" ? "↦ right" : "↧ bottom"}
            </button>
          </Tooltip>
        ) : null}
      </div>
      {attachError ? (
        <div className="border-b border-danger/40 bg-danger-soft px-3 py-1 text-micro text-danger-fg">
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
            <div className="flex h-full items-center justify-center p-6 text-center text-sm text-fg-muted">
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
            // Intentionally NOT including allow-same-origin: in the
            // internal/proxied scope, the iframe loads under the SPA
            // origin, and combining allow-scripts + allow-same-origin
            // would let workflow-published HTML remove the sandbox and
            // touch parent state (window.parent, localStorage,
            // cookies). The unique opaque origin a sandboxed iframe
            // gets without this token is exactly what we want.
            sandbox="allow-scripts allow-forms allow-popups"
          />
        ) : (
          <div className="flex h-full items-center justify-center p-6 text-center text-sm text-fg-muted">
            No preview URL yet. The workflow can publish one with{" "}
            <code className="mx-1 rounded bg-surface-2 px-1 font-mono">
              [iterion] preview_url=&lt;url&gt;
            </code>{" "}
            on a tool node's stdout, or you can type one above.
          </div>
        )}
      </div>
      {showLive ? null : isScrubbing ? (
        <div className="border-t border-border-default px-3 py-1 text-micro text-warning">
          scrubbed at seq {scrubSeq}
          {scrubbedShot
            ? ` · screenshot from ${scrubbedShot.nodeId ?? "unknown"} (seq ${scrubbedShot.seq})`
            : ""}
        </div>
      ) : sourceLabel ? (
        <div className="border-t border-border-default px-3 py-1 text-micro text-fg-muted">
          {sourceLabel}
          {browser.kind ? ` · ${browser.kind}` : null}
          {browser.scope === "internal" ? " · proxied" : null}
        </div>
      ) : null}
    </div>
  );
}
