// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import PreviewBody from "./PreviewBody";
import type { PreviewState } from "../usePreview";

afterEach(cleanup);

function state(over: Partial<PreviewState> = {}): PreviewState {
  return {
    path: "rgaa-2026-10-22.md",
    size: 42,
    loading: false,
    error: null,
    textBody: "# Rapport d'audit\n\nUn **constat**.",
    blobURL: null,
    contentType: "text/markdown; charset=utf-8",
    ...over,
  } as PreviewState;
}

describe("PreviewBody", () => {
  it("renders a markdown body as a document, not as source", () => {
    render(<PreviewBody preview={state()} />);
    expect(screen.getByRole("heading", { name: "Rapport d'audit" })).toBeTruthy();
    expect(screen.getByText("constat").tagName).toBe("STRONG");
    expect(document.querySelector("pre")).toBeNull();
  });

  it("toggles to the source and back with one label and a pressed state (RGAA 7.1)", () => {
    render(<PreviewBody preview={state()} />);
    const toggle = screen.getByRole("button", { name: "Show source", pressed: false });
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: "Show source", pressed: true })).toBeTruthy();
    expect(document.querySelector("pre")?.textContent).toContain("# Rapport d'audit");
    expect(screen.queryByRole("heading")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show source", pressed: true }));
    expect(screen.getByRole("button", { name: "Show source", pressed: false })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Rapport d'audit" })).toBeTruthy();
  });

  it("shows a plain text body as source, with no toggle", () => {
    render(
      <PreviewBody
        preview={state({ contentType: "text/plain; charset=utf-8", textBody: "# not markdown" })}
      />,
    );
    expect(document.querySelector("pre")?.textContent).toBe("# not markdown");
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.queryByRole("button", { name: "Show source" })).toBeNull();
  });

  it("keeps the loading and error states", () => {
    render(<PreviewBody preview={state({ loading: true, textBody: null })} />);
    expect(screen.getByText("Loading preview…")).toBeTruthy();
    cleanup();
    render(<PreviewBody preview={state({ error: "boom", textBody: null })} />);
    expect(screen.getByText("Failed to load: boom")).toBeTruthy();
  });
});
