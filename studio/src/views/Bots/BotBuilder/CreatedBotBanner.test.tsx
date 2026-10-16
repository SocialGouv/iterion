// @vitest-environment jsdom
//
// The builder's "is live" banner is the only screen a local-mode operator
// sees after creating a bot: it must not claim the new bundle is visible to
// the orchestrator catalog when the server could not refresh it.
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { BotCreateResult } from "@/api/bots";

import CreatedBotBanner from "./CreatedBotBanner";

afterEach(cleanup);

const cause =
  "botregistry: read catalog template /ws/bots/nexie/iterion-bot-catalog-static.md: too many levels of symbolic links";

function created(over: Partial<BotCreateResult> = {}): BotCreateResult {
  return {
    name: "expense-approval",
    display_name: "Expense approval",
    path: "/ws/bots/expense-approval",
    rel_path: "bots/expense-approval",
    ...over,
  } as BotCreateResult;
}

describe("CreatedBotBanner", () => {
  it("claims orchestrator visibility when the refresh succeeded, and nothing more", () => {
    const { container } = render(<CreatedBotBanner created={created()} isCloud={false} />);
    const shown = container.textContent ?? "";

    expect(shown).toContain("Expense approval is live");
    expect(shown).toContain("visible to the orchestrator catalog");
    expect(shown).not.toContain("not refreshed");
    expect(shown).not.toContain("regen-catalog");
  });

  it("names the cause and the retry, and drops the visibility claim, when the refresh failed", () => {
    const { container } = render(
      <CreatedBotBanner created={created({ catalog_error: cause })} isCloud={false} />,
    );
    const shown = container.textContent ?? "";

    expect(shown).toContain("Expense approval is live");
    expect(shown).not.toContain("visible to the orchestrator catalog");
    expect(shown).toContain("Catalog not refreshed");
    expect(shown).toContain(cause);
    expect(shown).toContain("iterion bots regen-catalog");
  });
});
