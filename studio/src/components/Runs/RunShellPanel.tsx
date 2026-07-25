import { useEffect, useRef, useState } from "react";

import { Drawer } from "@/components/ui";

// Post-mortem shell drawer: an xterm.js terminal bridged over
// GET /api/ws/runs/{id}/shell. PTY bytes ride BINARY frames both ways;
// resize/ping ride small JSON text frames (see pkg/server/runs_shell.go).
// One shell per mount — closing the drawer kills it; reopening spawns a
// fresh one (the worktree is the state, the shell is a viewer).
//
// xterm is imported lazily so its bundle+CSS only load when a shell is
// actually opened.

interface RunShellPanelProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  runId: string;
}

export function RunShellPanel({ open, onOpenChange, runId }: RunShellPanelProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const [status, setStatus] = useState<"connecting" | "live" | "closed" | "error">(
    "connecting",
  );

  useEffect(() => {
    if (!open) return;
    const host = hostRef.current;
    if (!host) return;

    let disposed = false;
    let ws: WebSocket | null = null;
    let term: import("@xterm/xterm").Terminal | null = null;
    let resizeObserver: ResizeObserver | null = null;

    void (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);
      await import("@xterm/xterm/css/xterm.css");
      if (disposed) return;

      term = new Terminal({
        convertEol: false,
        fontSize: 13,
        fontFamily:
          "'Geist Mono Variable', ui-monospace, SFMono-Regular, Menlo, monospace",
        cursorBlink: true,
        scrollback: 5000,
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(host);
      fit.fit();

      const proto = window.location.protocol === "https:" ? "wss" : "ws";
      const url = `${proto}://${window.location.host}/api/ws/runs/${encodeURIComponent(
        runId,
      )}/shell?cols=${term.cols}&rows=${term.rows}`;
      ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";

      ws.onopen = () => setStatus("live");
      ws.onclose = () => {
        setStatus((prev) => (prev === "error" ? prev : "closed"));
        term?.write("\r\n\x1b[2m[shell closed]\x1b[0m\r\n");
      };
      ws.onerror = () => setStatus("error");
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          // Control frame — exit/pong; nothing to render.
          return;
        }
        term?.write(new Uint8Array(ev.data as ArrayBuffer));
      };

      term.onData((data) => {
        if (ws?.readyState === WebSocket.OPEN) {
          ws.send(new TextEncoder().encode(data));
        }
      });

      resizeObserver = new ResizeObserver(() => {
        fit.fit();
        if (ws?.readyState === WebSocket.OPEN && term) {
          ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
        }
      });
      resizeObserver.observe(host);
    })();

    return () => {
      disposed = true;
      resizeObserver?.disconnect();
      ws?.close();
      term?.dispose();
      setStatus("connecting");
    };
  }, [open, runId]);

  return (
    <Drawer
      open={open}
      onOpenChange={onOpenChange}
      title="Post-mortem shell"
      description={
        status === "live"
          ? "Interactive shell in the run's preserved worktree. Closing this panel ends the shell."
          : status === "closed"
            ? "Shell ended — reopen the panel for a fresh one."
            : status === "error"
              ? "Shell connection failed — see the run's eligibility (terminal status + worktree on disk)."
              : "Connecting…"
      }
      widthClass="max-w-3xl"
    >
      <div
        ref={hostRef}
        className="h-[70vh] min-h-64 w-full overflow-hidden rounded-md border border-border-default bg-black/90 p-1"
      />
    </Drawer>
  );
}
