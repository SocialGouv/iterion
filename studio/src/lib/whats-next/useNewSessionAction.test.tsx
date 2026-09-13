// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "@/api/client";
import { useUIStore } from "@/store/ui";

import { useNewSessionAction } from "./useNewSessionAction";

const { cancelRunMock } = vi.hoisted(() => ({ cancelRunMock: vi.fn() }));
vi.mock("@/api/runs", () => ({ cancelRun: cancelRunMock }));

function Harness({ session }: { session: Record<string, unknown> }) {
  const action = useNewSessionAction({ bot: { label: "Copi" }, session: session as never });
  return (
    <>
      {action.dialog}
      <button type="button" disabled={action.busy} onClick={() => void action.start()}>
        new session
      </button>
    </>
  );
}

function makeSession(over: Record<string, unknown> = {}) {
  return {
    status: "active",
    runId: "run-1",
    runStatus: "running",
    newSession: vi.fn(),
    ...over,
  };
}

beforeEach(() => {
  cancelRunMock.mockReset();
  useUIStore.setState({ toasts: [] });
});

afterEach(cleanup);

describe("useNewSessionAction run disposal", () => {
  it("does not reset the UI when cancellation fails", async () => {
    const session = makeSession();
    cancelRunMock.mockRejectedValue(new ApiError(503, "down"));
    render(<Harness session={session} />);
    fireEvent.click(screen.getByRole("button", { name: "new session" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel and start new" }));

    await waitFor(() => expect(useUIStore.getState().toasts).toHaveLength(1));
    expect(session.newSession).not.toHaveBeenCalled();
    expect(useUIStore.getState().toasts[0]?.action?.label).toBe("Retry");
  });

  it("treats an already-gone run as successfully abandoned", async () => {
    const session = makeSession();
    cancelRunMock.mockRejectedValue(new ApiError(404, "gone"));
    render(<Harness session={session} />);
    fireEvent.click(screen.getByRole("button", { name: "new session" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel and start new" }));
    await waitFor(() => expect(session.newSession).toHaveBeenCalledOnce());
  });

  it("still asks the idempotent server to close a known terminal run", async () => {
    const session = makeSession({ status: "ended", runStatus: "finished" });
    cancelRunMock.mockResolvedValue({ run_id: "run-1", status: "finished" });
    render(<Harness session={session} />);
    fireEvent.click(screen.getByRole("button", { name: "new session" }));
    await waitFor(() => expect(session.newSession).toHaveBeenCalledOnce());
    expect(cancelRunMock).toHaveBeenCalledWith("run-1");
    expect(screen.queryByRole("button", { name: "Cancel and start new" })).toBeNull();
  });
});
