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
        setStatus("ready");

        stopOff = client.on("Page.screencastFrame", (params) => {
          const data = (params.data as string) ?? "";
          const meta = (params.metadata ?? {}) as {
            deviceWidth?: number;
            deviceHeight?: number;
          };
          const ackId = (params.sessionId ?? params["sessionId"]) as
            | number
            | string
            | undefined;
          drawFrame(data, meta).catch(() => undefined);
          // Always ack so Chromium keeps streaming.
          if (typeof ackId === "number") {
            void client
              .send("Page.screencastFrameAck", { sessionId: ackId })
              .catch(() => undefined);
          }
        });

        await client.send("Page.enable");
        await client.send("Page.startScreencast", {
          format: "jpeg",
          quality: 60,
          everyNthFrame: 2,
        });
      } catch (err) {
        if (cancelled) return;
        setErrorMsg(err instanceof Error ? err.message : String(err));
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
      void client
        .send("Page.stopScreencast")
        .catch(() => undefined)
        .finally(() => {
          client.close();
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
    if (!client || status !== "ready") return;
    const coords = toBrowserCoords(e);
    if (!coords) return;
    void client
      .send("Input.dispatchMouseEvent", {
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
      })
      .catch(() => undefined);
  };

  const dispatchKey = (
    type: "keyDown" | "keyUp",
    e: React.KeyboardEvent<HTMLCanvasElement>,
  ) => {
    const client = clientRef.current;
    if (!client || status !== "ready") return;
    void client
      .send("Input.dispatchKeyEvent", {
        type,
        key: e.key,
        code: e.code,
        // Chromium also accepts text + windowsVirtualKeyCode; this
        // minimal form is enough for printable + navigation keys.
      })
      .catch(() => undefined);
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-black">
      <div className="flex-1 min-h-0 overflow-auto flex items-center justify-center">
        {status === "error" ? (
          <div className="p-6 text-center text-sm text-red-400">
            CDP connection failed: {errorMsg}
          </div>
        ) : status === "connecting" ? (
          <div className="p-6 text-center text-sm text-text-2">
            Connecting to Chromium…
          </div>
        ) : null}
        <canvas
          ref={canvasRef}
          tabIndex={0}
          onMouseDown={(e) => dispatchMouse("mousePressed", e)}
          onMouseUp={(e) => dispatchMouse("mouseReleased", e)}
          onMouseMove={(e) => dispatchMouse("mouseMoved", e)}
          onKeyDown={(e) => dispatchKey("keyDown", e)}
          onKeyUp={(e) => dispatchKey("keyUp", e)}
          onContextMenu={(e) => e.preventDefault()}
          className={`max-h-full max-w-full bg-black focus:outline-2 focus:outline-accent ${
            status === "ready" ? "" : "hidden"
          }`}
        />
      </div>
      <div className="border-t border-border-default px-3 py-1 text-[11px] text-text-2 flex items-center gap-2">
        <span className="inline-block h-2 w-2 rounded-full bg-emerald-500" />
        live · session {sessionId.slice(0, 8)}
        {frameSize ? ` · ${frameSize.w}×${frameSize.h}` : null}
      </div>
    </div>
  );
}
