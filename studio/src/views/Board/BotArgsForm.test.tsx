// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { BotArgsForm } from "./BotArgsForm";

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
