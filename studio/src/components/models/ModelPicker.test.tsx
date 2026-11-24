// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { ModelEntry } from "@/api/models";

import ModelPicker from "./ModelPicker";

afterEach(cleanup);

// A gateway entry: the three capability flags are ABSENT when no catalog
// table answered them (source "unknown"), which is what the picker must word
// as unknown rather than as a refusal.
function gatewayEntry(patch: Partial<ModelEntry> = {}): ModelEntry {
  return {
    spec: "openai_compatible/gw-m",
    provider: "openai_compatible",
    model: "gw-m",
    credential_provider: "openai_compatible",
    source: "unknown",
    context_window: 200_000,
    ultracode_capable: false,
    price_known: false,
    usable: true,
    reachability: "cloud",
    ...patch,
  };
}

describe("ModelPicker capability wording", () => {
  it("words an unanswered tool_call as unknown, in the option and the detail line", () => {
    const m = gatewayEntry();
    render(<ModelPicker value={m.spec} onChange={() => {}} models={[m]} />);

    const option = screen.getByRole("option", { name: /openai_compatible\/gw-m/ });
    expect(option.textContent).toContain("tools unknown");
    expect(option.textContent).not.toContain("no tools");

    // The detail line words both flags the picker reads.
    expect(screen.getByText(/tools unknown · reasoning unknown/)).toBeTruthy();
  });

  it("still words a KNOWN false as a refusal", () => {
    const m = gatewayEntry({ tool_call: false, reasoning: false });
    render(<ModelPicker value={m.spec} onChange={() => {}} models={[m]} />);

    const option = screen.getByRole("option", { name: /openai_compatible\/gw-m/ });
    expect(option.textContent).toContain("no tools");
    expect(option.textContent).not.toContain("tools unknown");

    expect(screen.getByText(/no tools · no reasoning/)).toBeTruthy();
  });
});
