// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { BotDiscoveryError } from "@/api/bots";
import answer from "./__fixtures__/overlay-refused-bots.json";
import { DiscoveryErrorBanners } from "./DiscoveryErrorBanners";

afterEach(cleanup);

// Fed the server's own answer over a workspace overlay its reader refused
// (GET /api/v1/bots): every bot listed disabled, the refusal among the
// discovery errors. The gallery says it as what it is — every bot listed
// disabled, the doors that read visibility closed, and the remedy — never as
// one more skipped bot.
describe("DiscoveryErrorBanners over a refused overlay", () => {
  const errors = answer.discovery_errors as BotDiscoveryError[];

  it("lists every bot disabled, as the server answers", () => {
    expect(answer.bots.length).toBeGreaterThan(0);
    expect(answer.bots.every((b) => !b.enabled)).toBe(true);
  });

  it("says the overlay is not read, its consequence and its remedy", () => {
    render(<DiscoveryErrorBanners errors={errors} />);
    expect(screen.getByText("The workspace's bot visibility overlay is not read")).toBeTruthy();
    const body = document.body.textContent ?? "";
    expect(body).toContain("ITERION_DSL_READ_BUDGET");
    expect(body).toContain("listed disabled");
    expect(body).toContain(".iterion/bot-overrides.yaml");
  });

  it("does not count it among skipped bots", () => {
    render(<DiscoveryErrorBanners errors={errors} />);
    expect(screen.queryByText(/skipped$/)).toBeNull();
  });

  it("still counts a skipped bot beside it", () => {
    render(<DiscoveryErrorBanners errors={[...errors, { path: "bots/broken", error: "manifest: bad", kind: "bundle" }]} />);
    expect(screen.getByText("1 bot skipped")).toBeTruthy();
    expect(screen.getByText("The workspace's bot visibility overlay is not read")).toBeTruthy();
  });
});
