// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import EventLog from "./EventLog";

function stubStorage(initial: Record<string, string>) {
  const data = new Map(Object.entries(initial));
  const writes: Array<{ key: string; value: string }> = [];
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (key: string) => data.get(key) ?? null,
      setItem: (key: string, value: string) => {
        writes.push({ key, value });
        data.set(key, value);
      },
      removeItem: (key: string) => {
        data.delete(key);
      },
    },
  });
  return { data, writes };
}

function renderEventLog(runId: string | null) {
  return render(
    <EventLog
      events={[]}
      selectedExecutionId={null}
      followTail={false}
      onToggleFollow={vi.fn()}
      runId={runId}
    />,
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("EventLog persisted filters", () => {
  it("reloads filters when the run changes without remounting", async () => {
    const storage = stubStorage({
      "run-console.event-filters.v1.run-a": JSON.stringify({ search: "alpha" }),
      "run-console.event-filters.v1.run-b": JSON.stringify({ search: "beta" }),
    });

    const view = renderEventLog("run-a");
    const input = screen.getByPlaceholderText(/Search events/) as HTMLInputElement;
    expect(input.value).toBe("alpha");

    view.rerender(
      <EventLog
        events={[]}
        selectedExecutionId={null}
        followTail={false}
        onToggleFollow={vi.fn()}
        runId="run-b"
      />,
    );

    await waitFor(() => expect(input.value).toBe("beta"));
    expect(
      storage.writes.some(
        (write) =>
          write.key === "run-console.event-filters.v1.run-b" &&
          write.value.includes("alpha"),
      ),
    ).toBe(false);
  });

  it("ignores malformed persisted filter shapes", () => {
    stubStorage({
      "run-console.event-filters.v1.run-a": JSON.stringify({
        search: 42,
        types: "node_started",
      }),
    });

    renderEventLog("run-a");

    const input = screen.getByPlaceholderText(/Search events/) as HTMLInputElement;
    expect(input.value).toBe("");
  });
});

describe("EventLog search field name", () => {
  it("names its search field — a placeholder alone is not a label (RGAA 11.1)", () => {
    renderEventLog(null);
    expect(screen.getByRole("textbox", { name: "Search events" })).toBeTruthy();
  });
});
