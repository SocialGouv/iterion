// @vitest-environment jsdom
// The shell drawer's WebSocket frames. The server's shell speaks two frame
// classes (pkg/server/runs_shell.go): BINARY frames carry the PTY bytes, and
// small JSON text frames carry the control traffic — {"type":"pong"},
// {"type":"exit"} and the typed {"type":"error","message":...} the server
// sends right before a deliberate close (lifetime_reached, credential_refused,
// scope_lost). The panel renders the bytes, stays silent on pong/exit and on
// a frame that does not parse, renders the typed error message into the
// terminal and the drawer's status, and names the close reason on the close
// line when no error frame said why first.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { RunShellPanel } from "./RunShellPanel";

const written: (string | Uint8Array)[] = [];

vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    write = (data: string | Uint8Array): void => {
      written.push(data);
    };
    loadAddon = vi.fn();
    open = vi.fn();
    dispose = vi.fn();
    onData = vi.fn();
  },
}));

vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));

vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

class FakeWebSocket {
  static OPEN = 1;
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  binaryType = "blob";
  onopen: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  sent: (string | Uint8Array)[] = [];
  closeReason = "";
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }
  send(data: string | Uint8Array): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = 3;
    this.onclose?.(new CloseEvent("close", { reason: this.closeReason, code: 1008 }));
  }
  text(text: string): void {
    this.onmessage?.(new MessageEvent("message", { data: text }));
  }
  bytes(data: ArrayBuffer): void {
    this.onmessage?.(new MessageEvent("message", { data }));
  }
}

const SCOPE_LOST = "scope_lost: the run this socket watches is gone";

const text = (): string =>
  written
    .map((chunk) => (typeof chunk === "string" ? chunk : new TextDecoder().decode(chunk)))
    .join("");

// The drawer's terminal host mounts with the portal content, one commit after
// the panel's own effect ran, so the mount render spawns nothing; the second
// render — the shell for the run under test — is the one that opens. Exactly
// one WebSocket exists per witness: the run under test's.
function openPanel(): void {
  const view = render(
    <RunShellPanel open onOpenChange={() => undefined} runId="drawer-mount" />,
  );
  view.rerender(<RunShellPanel open onOpenChange={() => undefined} runId="review-123" />);
}

const theSocket = (): FakeWebSocket => FakeWebSocket.instances[0]!;

async function openedPanel(): Promise<FakeWebSocket> {
  openPanel();
  await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
  theSocket().open();
  return theSocket();
}

beforeEach(() => {
  written.length = 0;
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe = vi.fn();
      disconnect = vi.fn();
    },
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the shell panel's frames", () => {
  it("renders the typed error frame into the terminal and the status", async () => {
    const ws = await openedPanel();
    ws.text(JSON.stringify({ type: "error", message: SCOPE_LOST }));
    expect(text()).toContain(SCOPE_LOST);
    // Once the shell has ended, the drawer's status names the server's cause,
    // not a generic failure.
    ws.close();
    await screen.findByText(new RegExp(SCOPE_LOST));
  });

  it("answers nothing more on a close that an error frame already explained", async () => {
    const ws = await openedPanel();
    ws.text(JSON.stringify({ type: "error", message: SCOPE_LOST }));
    expect(text()).toContain(SCOPE_LOST);
    ws.closeReason = SCOPE_LOST;
    ws.close();
    expect(text()).not.toContain("[shell closed");
    // The status still carries the cause after the close.
    await screen.findByText(new RegExp(SCOPE_LOST));
  });

  it("writes the close reason when no error frame named it", async () => {
    const ws = await openedPanel();
    ws.closeReason = SCOPE_LOST;
    ws.close();
    expect(text()).toContain(`[shell closed: ${SCOPE_LOST}]`);
  });

  it("writes a plain close line when the close names no reason", async () => {
    const ws = await openedPanel();
    ws.close();
    expect(text()).toContain("[shell closed]");
    expect(text()).not.toContain("[shell closed:");
  });

  it("stays silent on pong and on a frame that does not parse, and renders bytes as terminal data", async () => {
    const ws = await openedPanel();
    ws.text(JSON.stringify({ type: "pong" }));
    ws.text("not json at all");
    ws.bytes(new TextEncoder().encode("pty$ ").buffer as ArrayBuffer);
    expect(text()).toBe("pty$ ");
  });
});
