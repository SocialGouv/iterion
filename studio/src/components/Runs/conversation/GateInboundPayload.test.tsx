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

import { TEXT_PREVIEW_BYTE_BUDGET } from "./gateInboundFold";
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

  it("skips a key whose value the rendered instructions already show", () => {
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{ reply: "the whole answer, already on screen", plan: "the plan" }}
        instructionInputs={["reply"]}
        instructionsText="the whole answer, already on screen"
      />,
    );
    expect(screen.getByText("the plan")).toBeTruthy();
    expect(screen.queryByText(/already on screen/)).toBeNull();
  });

  it("keeps that key on a surface that renders no instructions", () => {
    render(
      <GateInboundPayload
        runId="run-1"
        questions={{ reply: "the whole answer" }}
        instructionInputs={["reply"]}
      />,
    );
    expect(screen.getByText("the whole answer")).toBeTruthy();
  });

  // Folding prose is a height clamp, never a slice of the source: cutting
  // markdown at line 12 inside a fenced block renders an unterminated
  // fence that swallows the rest of the payload.
  it("folds long prose without truncating the markdown source", () => {
    const text = [
      "Here is the migration:",
      "```go",
      ...Array.from({ length: 15 }, (_, i) => `line${i} := true`),
      "```",
      "Ends with a closed fence.",
    ].join("\n");

    const { container } = render(
      <GateInboundPayload runId="run-1" questions={{ plan: text }} />,
    );

    // Folded (a toggle is offered)…
    expect(screen.getByRole("button", { name: /show all \d+ lines/i })).toBeTruthy();
    // …yet the whole source is rendered: the fence closed, so the trailing
    // paragraph is a paragraph and not code.
    const tail = screen.getByText("Ends with a closed fence.");
    expect(tail.closest("code")).toBeNull();
    expect(container.querySelector("code")?.textContent).toContain("line14 := true");
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

  it("previews a JSON attachment instead of offering only a download", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(['{"subject":"Ulysse","chapters":[1,2]}'], { type: "application/json" }),
      contentType: "application/json",
    });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          outline_file: {
            attachment: "outline_file-609b6784",
            filename: "outline.json",
            path: "/host/state/films/x/outline.json",
            mime: "application/json",
            size: 48,
          },
        }}
        inputFields={[{ name: "outline_file", type: "file" }]}
      />,
    );

    expect(await screen.findByText(/"subject": "Ulysse"/)).toBeTruthy();
    expect(screen.getByRole("link", { name: /download/i })).toBeTruthy();
    expect(screen.queryByRole("img")).toBeNull();
    expect(fetchAttachment).toHaveBeenCalledWith("run-1", "outline_file-609b6784");
  });

  it("does not inline a text attachment larger than the byte budget", async () => {
    const blob = new Blob(["do-not-decode"], { type: "text/plain" });
    Object.defineProperty(blob, "size", { value: TEXT_PREVIEW_BYTE_BUDGET + 1 });
    fetchAttachment.mockResolvedValue({ blob, contentType: "text/plain" });

    const { container } = render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          log_file: {
            attachment: "log-huge-bytes",
            filename: "build.log",
            mime: "text/plain",
            size: TEXT_PREVIEW_BYTE_BUDGET + 1,
          },
        }}
        inputFields={[{ name: "log_file", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("link", { name: /download/i })).toBeTruthy();
    expect(screen.queryByText("do-not-decode")).toBeNull();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();
    expect(container.querySelector("pre")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("previews an extension-less markdown file from the declared mime", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["# Brief to review"], { type: "application/octet-stream" }),
      contentType: "application/octet-stream",
    });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          brief: {
            attachment: "brief-noext",
            filename: "brief",
            mime: "text/markdown",
          },
        }}
        inputFields={[{ name: "brief", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("heading", { name: /brief to review/i })).toBeTruthy();
  });

  it("keeps a neutralized XML attachment as download even with a declared text/xml mime", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["<root/>"], { type: "application/octet-stream" }),
      contentType: "application/octet-stream",
    });
    const createObjectURL = vi.fn(() => "blob:xml");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          spec: {
            attachment: "spec-xml-declared",
            filename: "data.xml",
            mime: "text/xml",
          },
        }}
        inputFields={[{ name: "spec", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("link", { name: /download/i })).toBeTruthy();
    expect(screen.queryByText("<root/>")).toBeNull();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();

    vi.unstubAllGlobals();
  });

  it("previews a markdown brief as prose, not as a download-only row", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["# Découpage à valider\n\nCe que tu juges"], { type: "text/plain" }),
      contentType: "text/plain",
    });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          brief_file: {
            attachment: "brief_file-da9c85f7",
            filename: "outline_review.md",
            mime: "text/plain",
            size: 40,
          },
        }}
        inputFields={[{ name: "brief_file", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("heading", { name: /découpage à valider/i })).toBeTruthy();
    expect(screen.getByText("Ce que tu juges")).toBeTruthy();
  });

  it("folds a long JSON attachment behind a toggle", async () => {
    const chapters = Object.fromEntries(
      Array.from({ length: 20 }, (_, i) => [`ch_${i}`, { events: ["a", "b"] }]),
    );
    fetchAttachment.mockResolvedValue({
      blob: new Blob([JSON.stringify({ chapters })], { type: "application/json" }),
      contentType: "application/json",
    });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          outline_file: {
            attachment: "outline_file-long",
            filename: "outline.json",
            mime: "application/json",
          },
        }}
        inputFields={[{ name: "outline_file", type: "file" }]}
      />,
    );

    const toggle = await screen.findByRole("button", { name: /show all \d+ lines/i });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // Folded <pre> is a slice, not a CSS clamp over the whole body.
    expect(screen.queryByText(/ch_19/)).toBeNull();
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: /show less/i })).toBeTruthy();
    expect(screen.getByText(/ch_19/)).toBeTruthy();
  });

  it("pretty-prints a mid-size JSON outline and still slices the fold", async () => {
    const outline = {
      subject: "Ulysse",
      chapters: Array.from({ length: 400 }, (_, i) => ({
        id: `ch_${i}`,
        title: `Chapter ${i}`,
        events: ["land", "sea", "return"],
      })),
    };
    const raw = JSON.stringify(outline);
    expect(raw.length).toBeGreaterThan(20_000);
    expect(raw.length).toBeLessThan(2_000_000);

    fetchAttachment.mockResolvedValue({
      blob: new Blob([raw], { type: "application/json" }),
      contentType: "application/json",
    });

    const { container } = render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          outline_file: {
            attachment: "outline-mid",
            filename: "outline.json",
            mime: "application/json",
          },
        }}
        inputFields={[{ name: "outline_file", type: "file" }]}
      />,
    );

    const toggle = await screen.findByRole("button", { name: /show all \d+ lines/i });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    const pre = container.querySelector("pre");
    expect(pre?.textContent).toMatch(/\n\s+"subject": "Ulysse"/);
    expect(pre?.textContent).not.toBe(raw);
    expect(pre?.textContent?.length ?? 0).toBeLessThanOrEqual(20_000);
    expect(screen.queryByText(/"id": "ch_399"/)).toBeNull();

    fireEvent.click(toggle);
    expect(screen.getByText(/"id": "ch_399"/)).toBeTruthy();
  });

  it("folds a single-line attachment larger than the parse budget", async () => {
    const body = "x".repeat(25_000);
    fetchAttachment.mockResolvedValue({
      blob: new Blob([body], { type: "text/plain" }),
      contentType: "text/plain",
    });

    const { container } = render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          log_file: {
            attachment: "log-huge",
            filename: "trace.log",
            mime: "text/plain",
          },
        }}
        inputFields={[{ name: "log_file", type: "file" }]}
      />,
    );

    const toggle = await screen.findByRole("button", { name: /^show all$/i });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    const pre = container.querySelector("pre");
    expect(pre).toBeTruthy();
    expect(pre?.textContent?.length ?? 0).toBeLessThanOrEqual(20_000);
    expect(pre?.textContent?.length ?? 0).toBeLessThan(body.length);

    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: /show less/i })).toBeTruthy();
    expect(container.querySelector("pre")?.textContent).toBe(body);
  });

  it("does not inline-preview a neutralized XML attachment", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["<root/>"], { type: "application/octet-stream" }),
      contentType: "application/octet-stream",
    });
    const createObjectURL = vi.fn(() => "blob:xml");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          spec: {
            attachment: "spec-xml",
            filename: "data.xml",
            mime: "application/octet-stream",
          },
        }}
        inputFields={[{ name: "spec", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("link", { name: /download/i })).toBeTruthy();
    expect(screen.queryByText("<root/>")).toBeNull();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();

    vi.unstubAllGlobals();
  });

  it("leaves a zip as a download-only row", async () => {
    fetchAttachment.mockResolvedValue({
      blob: new Blob(["PK"], { type: "application/zip" }),
      contentType: "application/zip",
    });
    const createObjectURL = vi.fn(() => "blob:zip");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });

    render(
      <GateInboundPayload
        runId="run-1"
        questions={{
          bundle: {
            attachment: "bundle-zip",
            filename: "bundle.zip",
            mime: "application/zip",
          },
        }}
        inputFields={[{ name: "bundle", type: "file" }]}
      />,
    );

    expect(await screen.findByRole("link", { name: /download/i })).toBeTruthy();
    expect(screen.queryByRole("img")).toBeNull();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();
    expect(screen.queryByText("PK")).toBeNull();

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
