import { errorMessage } from "@/lib/errorHints";
import { useEffect, useRef, useState } from "react";

import { CDPClient } from "@/lib/cdpClient";

interface BrowserLivePaneProps {
  runId: string;
  sessionId: string;
}

// BrowserLivePane streams a live Chromium session to a <canvas> via
// CDP `Page.startScreencast` and forwards mouse/keyboard input back
// through `Input.dispatchMouseEvent` / `Input.dispatchKeyEvent`.
//
// Frame format: CDP returns base64-encoded JPEG (default; we ask
// for q60 to halve the egress vs PNG). We decode each frame to an
// ImageBitmap and blit; this is allocation-light and lets the
// browser's GPU compositor do the heavy lifting.
//
// Lifecycle: the component owns one CDPClient. On mount it connects,
// starts the screencast, and registers handlers. On unmount it stops
// the screencast and closes the WS — the frame loop self-terminates.
export default function BrowserLivePane({ runId, sessionId }: BrowserLivePaneProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const clientRef = useRef<CDPClient | null>(null);
  // pageSessionRef holds the flat-mode CDP sessionId returned by
  // Target.attachToTarget. All Page.*/Input.* calls must include it.
  const pageSessionRef = useRef<string | null>(null);
  const [status, setStatus] = useState<
    "connecting" | "ready" | "error" | "closed"
  >("connecting");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [frameSize, setFrameSize] = useState<{ w: number; h: number } | null>(
    null,
  );

  useEffect(() => {
    const client = new CDPClient({ runId, sessionId });
    clientRef.current = client;
    let cancelled = false;
    let stopOff: (() => void) | null = null;

    (async () => {
      try {
        await client.connect();
        if (cancelled) return;

        // CDP `--remote-debugging-pipe` exposes the browser-level
        // session, which only carries Browser/Target/Tracing domains.
        // Page.* live on a page target's session. Find (or create) a
        // page target, attach in flat mode, and route subsequent
        // Page.* calls through that sessionId.
        const targetsResp = await client.send("Target.getTargets");
        const infos = (targetsResp.targetInfos ?? []) as Array<{
          targetId: string;
          type: string;
        }>;
        let pageTargetId =
          infos.find((t) => t.type === "page")?.targetId ?? null;
        if (!pageTargetId) {
          // Fresh browser without an open tab: create one. about:blank
          // renders as a white page so the canvas isn't an empty void.
          const created = (await client.send("Target.createTarget", {
            url: "about:blank",
          })) as { targetId?: string };
          pageTargetId = created.targetId ?? null;
        }
        if (!pageTargetId) {
          throw new Error("cdp: could not find or create a page target");
        }
        const attachResp = (await client.send("Target.attachToTarget", {
          targetId: pageTargetId,
          flatten: true,
        })) as { sessionId?: string };
        const pageSessionId = attachResp.sessionId;
        if (!pageSessionId) {
          throw new Error("cdp: Target.attachToTarget returned no sessionId");
        }
        pageSessionRef.current = pageSessionId;

        if (cancelled) return;
        setStatus("ready");

        stopOff = client.on("Page.screencastFrame", (params) => {
          const data = (params.data as string) ?? "";
          const meta = (params.metadata ?? {}) as {
            deviceWidth?: number;
            deviceHeight?: number;
          };
          // Chromium's frame ack id is `sessionId` *inside* the params
          // (it's the screencast session, NOT the CDP attach session).
          const ackId = params.sessionId as number | string | undefined;
          drawFrame(data, meta).catch(() => undefined);
          if (typeof ackId === "number") {
            void client
              .send(
                "Page.screencastFrameAck",
                { sessionId: ackId },
                pageSessionId,
              )
              .catch(() => undefined);
          }
        });

        await client.send("Page.enable", {}, pageSessionId);
        await client.send(
          "Page.startScreencast",
          { format: "jpeg", quality: 60, everyNthFrame: 2 },
          pageSessionId,
        );
      } catch (err) {
        if (cancelled) return;
        setErrorMsg(errorMessage(err));
        setStatus("error");
      }
    })();

    const drawFrame = async (
      base64Data: string,
      meta: { deviceWidth?: number; deviceHeight?: number },
    ) => {
      const canvas = canvasRef.current;
      if (!canvas || !base64Data) return;
      // base64 → bytes → Blob → ImageBitmap, the cheap path the
      // browser optimises for GPU upload.
      const bin = atob(base64Data);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      const blob = new Blob([bytes], { type: "image/jpeg" });
      const bitmap = await createImageBitmap(blob);
      if (cancelled) {
        bitmap.close?.();
        return;
      }
      if (
        meta.deviceWidth &&
        meta.deviceHeight &&
        (canvas.width !== meta.deviceWidth ||
          canvas.height !== meta.deviceHeight)
      ) {
        canvas.width = meta.deviceWidth;
        canvas.height = meta.deviceHeight;
        setFrameSize({ w: meta.deviceWidth, h: meta.deviceHeight });
      }
      const ctx = canvas.getContext("2d");
      if (ctx) {
        ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
      }
      bitmap.close?.();
    };

    return () => {
      cancelled = true;
      if (stopOff) stopOff();
      const pageSession = pageSessionRef.current ?? undefined;
      void client
        .send("Page.stopScreencast", {}, pageSession)
        .catch(() => undefined)
        .finally(() => {
          client.close();
          pageSessionRef.current = null;
          setStatus("closed");
        });
    };
  }, [runId, sessionId]);

  // Translate canvas-relative pointer events to CDP-virtual coords.
  // Chromium's coordinate space is the device pixel grid (frameSize);
  // the canvas may be styled larger or smaller, so we rescale.
  const toBrowserCoords = (e: React.MouseEvent<HTMLCanvasElement>) => {
    const canvas = canvasRef.current;
    if (!canvas || !frameSize) return null;
    const rect = canvas.getBoundingClientRect();
    const x = ((e.clientX - rect.left) * frameSize.w) / rect.width;
    const y = ((e.clientY - rect.top) * frameSize.h) / rect.height;
    return { x: Math.round(x), y: Math.round(y) };
  };

  const dispatchMouse = (
    type: "mousePressed" | "mouseReleased" | "mouseMoved",
    e: React.MouseEvent<HTMLCanvasElement>,
  ) => {
    const client = clientRef.current;
    const pageSession = pageSessionRef.current;
    if (!client || !pageSession || status !== "ready") return;
    const coords = toBrowserCoords(e);
    if (!coords) return;
    void client
      .send(
        "Input.dispatchMouseEvent",
        {
          type,
          x: coords.x,
          y: coords.y,
          button:
            type === "mouseMoved"
              ? "none"
              : e.button === 2
                ? "right"
                : e.button === 1
                  ? "middle"
                  : "left",
          clickCount: type === "mousePressed" ? 1 : 0,
        },
        pageSession,
      )
      .catch(() => undefined);
  };

  const dispatchKey = (
    type: "keyDown" | "keyUp",
    e: React.KeyboardEvent<HTMLCanvasElement>,
  ) => {
    const client = clientRef.current;
    const pageSession = pageSessionRef.current;
    if (!client || !pageSession || status !== "ready") return;
    void client
      .send(
        "Input.dispatchKeyEvent",
        { type, key: e.key, code: e.code },
        pageSession,
      )
      .catch(() => undefined);
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-black">
      <div className="flex-1 min-h-0 overflow-auto flex items-center justify-center">
        {status === "error" ? (
          <div className="p-6 text-center text-sm text-danger">
            CDP connection failed: {errorMsg}
          </div>
        ) : status === "connecting" ? (
          <div className="p-6 text-center text-sm text-fg-muted">
            Connecting to Chromium…
          </div>
        ) : null}
        {/* The mirrored screen is an information-bearing image (RGAA
            1.1): role="img" puts the name and the image role on the
            same element, the label names WHAT is mirrored and follows
            the live frame size, and the child text is the fallback the
            canvas spec prescribes. */}
        <canvas
          ref={canvasRef}
          tabIndex={0}
          role="img"
          aria-label={`Live view of the run's browser session ${sessionId.slice(0, 8)}${frameSize ? ` — ${frameSize.w}×${frameSize.h}` : ""}`}
          onMouseDown={(e) => dispatchMouse("mousePressed", e)}
          onMouseUp={(e) => dispatchMouse("mouseReleased", e)}
          onMouseMove={(e) => dispatchMouse("mouseMoved", e)}
          onKeyDown={(e) => dispatchKey("keyDown", e)}
          onKeyUp={(e) => dispatchKey("keyUp", e)}
          onContextMenu={(e) => e.preventDefault()}
          className={`max-h-full max-w-full bg-black focus:outline-2 focus:outline-accent ${
            status === "ready" ? "" : "hidden"
          }`}
        >
          Live mirror of the browser session the run's browser tools
          drive. Your browser cannot display it.
        </canvas>
      </div>
      <div className="border-t border-border-default px-3 py-1 text-micro text-fg-muted flex items-center gap-2">
        <span className="inline-block h-2 w-2 rounded-full bg-success" />
        live · session {sessionId.slice(0, 8)}
        {frameSize ? ` · ${frameSize.w}×${frameSize.h}` : null}
      </div>
    </div>
  );
}
