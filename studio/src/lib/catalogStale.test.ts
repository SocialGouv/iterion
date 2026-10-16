import { describe, expect, it } from "vitest";

import { catalogStaleSuffix, catalogToggleToast } from "./catalogStale";

const regenFailure =
  "botregistry: read catalog template /ws/bots/nexie/iterion-bot-catalog-static.md: too many levels of symbolic links";

describe("catalogStaleSuffix", () => {
  it("names the cause and the command that retries the refresh", () => {
    const suffix = catalogStaleSuffix({ catalog_error: regenFailure });
    expect(suffix).toContain(regenFailure);
    expect(suffix).toContain("iterion bots regen-catalog");
  });

  it("is empty when the refresh succeeded, was skipped, or there is no result", () => {
    expect(catalogStaleSuffix({})).toBe("");
    expect(catalogStaleSuffix({ catalog_error: "" })).toBe("");
    expect(catalogStaleSuffix({ catalog_error: "   " })).toBe("");
    expect(catalogStaleSuffix(null)).toBe("");
    expect(catalogStaleSuffix(undefined)).toBe("");
  });
});

describe("catalogToggleToast", () => {
  it("confirms the new state, and nothing more, when the refresh succeeded", () => {
    expect(catalogToggleToast("Featurly enabled — exposed to Nexie", {})).toBe(
      "Featurly enabled — exposed to Nexie",
    );
    expect(catalogToggleToast("Featurly enabled — exposed to Nexie", null)).toBe(
      "Featurly enabled — exposed to Nexie",
    );
  });

  it("carries the stale routing table a failed refresh leaves, since enabled decides which rows it holds", () => {
    const msg = catalogToggleToast("Featurly enabled — exposed to Nexie", {
      catalog_error: regenFailure,
    });
    expect(msg).toContain("Featurly enabled — exposed to Nexie");
    expect(msg).toContain(regenFailure);
    expect(msg).toContain("iterion bots regen-catalog");
  });
});
