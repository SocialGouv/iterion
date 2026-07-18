// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import CronField, { CRON_PRESETS } from "./CronField";

afterEach(cleanup);

describe("CronField", () => {
  it("renders the built-in label and a live humanized preview", () => {
    render(<CronField value="0 2 * * *" onChange={() => {}} />);
    expect(screen.getByText(/Cron \(5-field, UTC/)).toBeTruthy();
    expect(screen.getByText("daily at 02:00")).toBeTruthy();
  });

  it("falls back to the raw-expression note on an unrecognized shape", () => {
    render(<CronField value="not a cron" onChange={() => {}} />);
    expect(
      screen.getByText("Unrecognized shape — the raw expression is used as-is."),
    ).toBeTruthy();
  });

  it("applies a preset via onChange", () => {
    const onChange = vi.fn();
    render(<CronField value="" onChange={onChange} />);
    const preset = CRON_PRESETS[0]!;
    fireEvent.click(screen.getByRole("button", { name: preset.label }));
    expect(onChange).toHaveBeenCalledWith(preset.cron);
  });

  it("propagates typed input via onChange", () => {
    const onChange = vi.fn();
    render(<CronField value="" onChange={onChange} />);
    fireEvent.change(screen.getByPlaceholderText("0 2 * * *"), {
      target: { value: "0 * * * *" },
    });
    expect(onChange).toHaveBeenCalledWith("0 * * * *");
  });

  it("hideLabel suppresses the FieldLabel and ariaLabel names the input", () => {
    render(
      <CronField
        value="0 2 * * *"
        onChange={() => {}}
        hideLabel
        ariaLabel="Cron schedule for Vigie"
      />,
    );
    expect(screen.queryByText(/Cron \(5-field, UTC/)).toBeNull();
    expect(screen.getByLabelText("Cron schedule for Vigie")).toBeTruthy();
  });

  it("disabled reaches the input and the preset buttons", () => {
    render(<CronField value="0 2 * * *" onChange={() => {}} disabled />);
    expect(
      (screen.getByPlaceholderText("0 2 * * *") as HTMLInputElement).disabled,
    ).toBe(true);
    for (const p of CRON_PRESETS) {
      expect(
        (screen.getByRole("button", { name: p.label }) as HTMLButtonElement).disabled,
      ).toBe(true);
    }
  });
});
