// @vitest-environment jsdom
//
// Opening a connection starts its backend (a local daemon can take seconds,
// or fail). A failure is shown, not left in the console with a menu that
// looks like it did nothing; and a second open of a connection still opening
// shares the first one instead of starting a second backend.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const desktop = vi.hoisted(() => ({
  isFirstRunPending: vi.fn(),
  listConnections: vi.fn(),
  getOpenConnections: vi.fn(),
  getCurrentProject: vi.fn(),
  openConnection: vi.fn(),
  closeConnection: vi.fn(),
  pickProjectDirectory: vi.fn(),
  addProjectSilently: vi.fn(),
}));
vi.mock("@/lib/desktopBridge", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/desktopBridge")>()),
  desktop,
  isBrowserWorkspace: () => false,
  onDesktopEvent: () => () => {},
}));

import WorkspaceShell from "./WorkspaceShell";

const projects = [
  { id: "a", name: "alpha", dir: "/a" },
  { id: "b", name: "beta", dir: "/b" },
  { id: "c", name: "gamma", dir: "/c" },
];

function bootWithAlphaOpen() {
  desktop.isFirstRunPending.mockResolvedValue(false);
  desktop.listConnections.mockResolvedValue(projects);
  desktop.getOpenConnections.mockResolvedValue(["a"]);
  render(<WorkspaceShell />);
}

// deferredOpens hands out one pending promise per call, settled by the test.
function deferredOpens() {
  const pending: Record<string, { resolve: () => void; reject: (e: unknown) => void }> = {};
  desktop.openConnection.mockImplementation(
    (id: string) =>
      new Promise<void>((resolve, reject) => {
        pending[id] = { resolve, reject };
      }),
  );
  return (id: string) => {
    const p = pending[id];
    if (!p) throw new Error(`no open of ${id} is pending`);
    return p;
  };
}

async function openFromMenu(name: string) {
  if (!screen.queryByText("+ Add local project…")) {
    fireEvent.click(await screen.findByLabelText("Open a connection"));
  }
  fireEvent.click(await screen.findByText(name));
}

afterEach(() => {
  cleanup();
  vi.resetAllMocks(); // also drops a *Once queue a failed test left behind
});

describe("opening a connection from the + menu", () => {
  it("shows why it failed, until dismissed", async () => {
    bootWithAlphaOpen();
    desktop.openConnection.mockRejectedValue("spawn daemon for /b: daemon for /b exited before serving");

    await openFromMenu("beta");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("exited before serving");
    fireEvent.click(screen.getByLabelText("Dismiss"));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps a failure on screen when another connection opens, and clears it when that one does", async () => {
    bootWithAlphaOpen();
    const pending = deferredOpens();

    await openFromMenu("beta");
    await openFromMenu("gamma");
    await act(async () => pending("b").reject("beta failed"));
    await screen.findByRole("alert");
    await act(async () => pending("c").resolve());
    expect(screen.getByRole("alert").textContent).toContain("beta failed");

    await openFromMenu("beta");
    await act(async () => pending("b").resolve());
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("lets a second open through after one that threw synchronously", async () => {
    bootWithAlphaOpen();
    desktop.openConnection.mockImplementation(() => {
      throw new Error("binding missing");
    });

    await openFromMenu("beta");
    await screen.findByRole("alert");
    await openFromMenu("beta");

    await waitFor(() => expect(desktop.openConnection).toHaveBeenCalledTimes(2));
  });

  it("shows a failed Add local project, and clears it on the next attempt", async () => {
    bootWithAlphaOpen();
    desktop.pickProjectDirectory.mockRejectedValueOnce("picker exploded").mockResolvedValueOnce("");

    fireEvent.click(await screen.findByLabelText("Open a connection"));
    fireEvent.click(await screen.findByText("+ Add local project…"));
    expect((await screen.findByRole("alert")).textContent).toContain("picker exploded");

    fireEvent.click(await screen.findByLabelText("Open a connection"));
    fireEvent.click(await screen.findByText("+ Add local project…"));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("names an empty rejection instead of leaving the banner blank", async () => {
    bootWithAlphaOpen();
    desktop.openConnection.mockRejectedValue(undefined);

    await openFromMenu("beta");

    expect((await screen.findByRole("alert")).textContent).toBe("Could not open the connection: unknown error×");
  });

  it("reads an object rejection as its message", async () => {
    bootWithAlphaOpen();
    desktop.openConnection.mockRejectedValue({ message: "daemon refused" });

    await openFromMenu("beta");

    expect((await screen.findByRole("alert")).textContent).toBe("Could not open the connection: daemon refused×");
  });

  it("opens each of two connections once while both are opening", async () => {
    bootWithAlphaOpen();
    const pending = deferredOpens();

    await openFromMenu("beta");
    await openFromMenu("gamma");
    await waitFor(() => expect(screen.getAllByText("Opening…")).toHaveLength(2));
    const beta = screen.getByText("beta").closest("button") as HTMLButtonElement;
    expect(beta.disabled).toBe(true);
    fireEvent.click(beta);

    expect(desktop.openConnection.mock.calls.map(([id]) => id)).toEqual(["b", "c"]);
    await act(async () => {
      pending("b").resolve();
      pending("c").resolve();
    });
  });

  it("shares an open still running with an Add local project that lands on it", async () => {
    bootWithAlphaOpen();
    const pending = deferredOpens();
    desktop.pickProjectDirectory.mockResolvedValue("/b");
    desktop.addProjectSilently.mockResolvedValue(projects[1]);

    await openFromMenu("beta");
    await screen.findByText("Opening…");
    fireEvent.click(screen.getByText("+ Add local project…"));
    await waitFor(() => expect(desktop.addProjectSilently).toHaveBeenCalled());
    await act(async () => {});

    expect(desktop.openConnection).toHaveBeenCalledTimes(1);
    await act(async () => pending("b").resolve());
  });
});
