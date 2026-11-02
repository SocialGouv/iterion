// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import OnePrompt from "@/views/CloudHome/OnePrompt";

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

  // What the visitor copies must be what the block displays. The payload is
  // bound to the RENDERED text, not to a constant, so the invariant holds in
  // both directions: truncating the button's value OR amputating the <pre>
  // turns this red. Seen red against both mutants before being trusted. (The
  // parity of the source copies behind that text is scripts/prompt-parity.sh,
  // run by `task prompt:parity`.)
  it("copies exactly the prompt the block displays", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    const { container } = render(<OnePrompt />);
    const displayed = container.querySelector("pre")?.textContent ?? "";
    expect(displayed).toContain("iterion remote login");
    fireEvent.click(screen.getByRole("button", { name: /cloud setup prompt/i }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(displayed));
  });

  it("links the full walkthrough in the documentation site", () => {
    render(<OnePrompt />);
    const link = screen.getByRole("link", { name: /full walkthrough/i });
    expect(link.getAttribute("href")).toBe(
      "https://socialgouv.github.io/iterion/getting-started.html",
    );
  });
});
