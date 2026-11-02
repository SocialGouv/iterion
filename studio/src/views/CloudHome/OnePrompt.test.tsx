// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import OnePrompt, { SETUP_PROMPT } from "@/views/CloudHome/OnePrompt";

// The block a visitor pastes is the product of this section: the guards
// assert on what is RENDERED, not on the exported constant — a JSX refactor
// that drops the block (or a copy edit that renames it into a comment) stays
// red here. The full four-way parity of the prompt copies is
// scripts/prompt-parity.sh, run by `task prompt:parity`.
describe("the one-prompt onboarding section of the product home", () => {
  afterEach(() => cleanup());

  it("renders the cloud setup prompt the visitor pastes", () => {
    const { container } = render(<OnePrompt />);
    expect(container.textContent).toContain(
      "Set up Iterion Cloud (https://iterion.cloud",
    );
    expect(container.textContent).toContain(
      "iterion remote login https://iterion.cloud",
    );
    // The funding step: a visitor already covered by the pool needs no key,
    // so the block must say the credential is conditional, not mandatory,
    // and name the wire ids the CLI actually accepts.
    expect(container.textContent).toContain("already funded");
    expect(container.textContent).toContain("wire ids");
    expect(container.textContent).toContain("moonshot");
  });

  it("names the section for the cloud onboarding, not a generic quickstart", () => {
    render(<OnePrompt />);
    expect(
      screen.getByRole("heading", { name: /One prompt/i }),
    ).toBeTruthy();
    expect(screen.getByText("GET STARTED WITH ITERION CLOUD")).toBeTruthy();
  });

  // One copy control, and its accessible name says what it copies: a bare
  // "copy" would collide with every other control on the page for a screen
  // reader (RGAA 11.1).
  it("offers a copy control whose accessible name names the prompt", () => {
    render(<OnePrompt />);
    expect(
      screen.getByRole("button", { name: /cloud setup prompt/i }),
    ).toBeTruthy();
  });

  // What the visitor copies must be what the block displays — the one
  // invariant the rendering checks above cannot see: a refactor that keeps
  // the <pre> intact but truncates or substitutes the button's value passes
  // every other assertion in this file. This stub was seen RED against a
  // truncated `value` before being trusted.
  it("copies the whole displayed prompt to the clipboard", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    render(<OnePrompt />);
    fireEvent.click(screen.getByRole("button", { name: /cloud setup prompt/i }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SETUP_PROMPT));
  });

  it("links the full walkthrough in the documentation site", () => {
    render(<OnePrompt />);
    const link = screen.getByRole("link", { name: /full walkthrough/i });
    expect(link.getAttribute("href")).toBe(
      "https://socialgouv.github.io/iterion/getting-started.html",
    );
  });
});
