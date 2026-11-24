// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BotArgsForm } from "./BotArgsForm";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

setupMatchMedia();

afterEach(cleanup);

describe("BotArgsForm", () => {
  it("associates each schema field label with its input", () => {
    const bot = {
      name: "rgaa-audit",
      vars: { fields: [{ name: "region", type: "string" }] },
    } as never;
    render(<BotArgsForm bot={bot} values={{}} onChange={vi.fn()} />);
    const input = screen.getByLabelText(/region/) as HTMLInputElement;
    const label = screen.getByText("region", { selector: "span" }).closest("label");
    expect((label as HTMLLabelElement).htmlFor).toBe(input.id);
  });
});

describe("a11y / board BotArgsForm", () => {
  // Both halves at once, each with TWO rows: the schema-declared fields and
  // the custom key/value list repeat the same controls per row, and one row
  // cannot show whether their names say which row they act on.
  it("has no violation with schema fields and custom args side by side", async () => {
    const bot = {
      name: "rgaa-audit",
      vars: {
        fields: [
          { name: "region", type: "string", required: true },
          { name: "depth", type: "int" },
          { name: "dry_run", type: "bool" },
        ],
      },
    } as never;
    const { container } = render(
      <BotArgsForm
        bot={bot}
        values={{ region: "fr", stale_key: "1", other_key: "2" }}
        onChange={vi.fn()}
      />,
    );
    // The two custom rows are the ones that repeat; assert their names before
    // the audit, so a regression reads as "the name went generic" and not as
    // an axe rule firing somewhere in the form.
    for (const key of ["stale_key", "other_key"]) {
      expect(screen.getByRole("textbox", { name: `Key for ${key}` })).toBeTruthy();
      expect(screen.getByRole("textbox", { name: `Value for ${key}` })).toBeTruthy();
      expect(screen.getByRole("button", { name: `Remove ${key}` })).toBeTruthy();
    }
    await expectNoViolationsStrict(container, "BotArgsForm");
  });

  it("has no violation with no bot picked at all (custom args only)", async () => {
    const { container } = render(
      <BotArgsForm bot={null} values={{}} onChange={vi.fn()} />,
    );
    await expectNoViolationsStrict(container, "BotArgsForm (no bot)");
  });
});
