// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const fetchAttachment = vi.fn();

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return {
    ...actual,
    fetchAttachment: (...args: unknown[]) => fetchAttachment(...args),
  };
});

import GateInboundPayload from "./GateInboundPayload";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("GateInboundPayload", () => {
  it("renders the gate's inbound payload with humanized labels", () => {
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          plan: "## Steps\n\nShip the migration",
          review_notes: "Watch the rollback path",
        }}
      />,
    );
    expect(screen.getByRole("heading", { name: /what you're reviewing/i })).toBeTruthy();
    expect(screen.getByText("Plan")).toBeTruthy();
    expect(screen.getByText("Review notes")).toBeTruthy();
    expect(screen.getByText("Ship the migration")).toBeTruthy();
    expect(screen.getByText("Watch the rollback path")).toBeTruthy();
  });

  it("renders nothing when the payload is empty or pure plumbing", () => {
    const { container, rerender } = render(
      <GateInboundPayload runId="run-1" questions={{}} />,
    );
    expect(container.firstChild).toBeNull();

    rerender(
      <GateInboundPayload
        runId="run-1"
        questions={{
          _queued_operator_messages: ["ping"],
          _permission: { tool: "Bash" },
        }}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("never shows plumbing keys alongside real content", () => {
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{ plan: "Ship it", _queued_operator_messages: ["hurry up"] }}
      />,
    );
    expect(screen.getByText("Ship it")).toBeTruthy();
    expect(screen.queryByText(/hurry up/)).toBeNull();
    expect(screen.queryByText(/queued operator messages/i)).toBeNull();
  });

  it("pretty-prints a structured value and folds a long one behind a toggle", () => {
    const findings = Array.from({ length: 20 }, (_, i) => ({ id: i, sev: "high" }));
    render(<GateInboundPayload runId="run-1" questions={{ findings }} />);

    const toggle = screen.getByRole("button", { name: /show all \d+ lines/i });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // Folded: the tail of the array is not in the DOM yet.
    expect(screen.queryByText(/"id": 19/)).toBeNull();

    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: /show less/i })).toBeTruthy();
    expect(screen.getByText(/"id": 19/)).toBeTruthy();
  });

  it("shows a short payload expanded — no click needed to read it", () => {
    render(<GateInboundPayload runId="run-1" questions={{ counts: { high: 2 } }} />);
    expect(screen.getByText(/"high": 2/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();
  });

  it("previews an image-typed field instead of printing its path", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["fake-png"], { type: "image/png" }),
      contentType: "image/png",
    });
    // jsdom has no createObjectURL.
    const createObjectURL = vi.fn(() => "blob:mockup");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          mockup: {
            attachment: "gate.mockup",
            filename: "sketch.png",
            // The descriptor's path is where the RUNNING NODES read the
            // bytes — never fetchable from a browser, never displayed as
            // the preview source.
            path: "/run/iterion/attachments/gate.mockup/sketch.png",
            mime: "image/png",
            size: 2048,
          },
        }}
        inputFields={[{ name: "mockup", type: "file" }]}
      />,
    );

    const img = await screen.findByRole("img", { name: "sketch.png" });
    expect(img.getAttribute("src")).toBe("blob:mockup");
    expect(fetchAttachment).toHaveBeenCalledWith("run-1", "gate.mockup");
    expect(screen.queryByText(/run\/iterion\/attachments/)).toBeNull();

    vi.unstubAllGlobals();
  });

  it("degrades to the filename when a file value has no fetchable attachment", async () => {
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{ spec: "docs/spec.md" }}
        inputFields={[{ name: "spec", type: "file" }]}
      />,
    );
    expect(screen.getByText("spec.md")).toBeTruthy();
    // Nothing to fetch — an unfetchable path must not produce a request
    // (or a broken <img>).
    await waitFor(() => expect(fetchAttachment).not.toHaveBeenCalled());
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("surfaces a failed attachment fetch with a download fallback", async () => {
    fetchAttachment.mockRejectedValue(new Error("API error 404: attachment not found"));
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{ mockup: { attachment: "gate.mockup", filename: "sketch.png" } }}
        inputFields={[{ name: "mockup", type: "file" }]}
      />,
    );
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText(/attachment not found/)).toBeTruthy();
    expect(screen.getByRole("link", { name: /download instead/i })).toBeTruthy();
  });
});
