// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import type { CodemodeLimits } from "@/api/runs";

import CodemodeSection from "./CodemodeSection";
import { emptyCodemodeFieldValues } from "./codemodePayload";

afterEach(cleanup);

const limits: CodemodeLimits = {
  default: {
    tool_calls: 64,
    wall: "30s",
    cpu: "10s",
    memory: 268435456,
    script_bytes: 262144,
    result_bytes: 1048576,
    tool_result_bytes: 65536,
  },
  ceiling: {
    tool_calls: 4096,
    wall: "10m0s",
    cpu: "5m0s",
    memory: 2147483648,
    script_bytes: 4194304,
    result_bytes: 8388608,
    tool_result_bytes: 1048576,
  },
  memory_floor: 67108864,
};

function renderSection(over: Partial<Parameters<typeof CodemodeSection>[0]> = {}) {
  const props = {
    mode: "",
    values: emptyCodemodeFieldValues,
    show: true,
    limits,
    routeRefusals: {} as Record<string, string>,
    onModeChange: vi.fn(),
    onToggle: vi.fn(),
    onChange: vi.fn(),
    ...over,
  };
  return { props, ...render(<CodemodeSection {...props} />) };
}

test("the mode select offers inherit, on and off — and reports the choice", () => {
  const { props } = renderSection();
  const select = screen.getByRole("combobox");
  fireEvent.change(select, { target: { value: "off" } });
  expect(props.onModeChange).toHaveBeenCalledWith("off");
  expect(screen.getByText(/off — keep it away for this run/)).toBeTruthy();
});

test("placeholders come from the served defaults, never from a studio table", () => {
  renderSection();
  expect(screen.getByLabelText(/tool_calls/i).getAttribute("placeholder")).toBe("64");
  expect(screen.getByLabelText(/^wall/i).getAttribute("placeholder")).toBe("30s");
  expect(screen.getByLabelText(/^memory/i).getAttribute("placeholder")).toBe(
    "268435456",
  );
});

test("without the served limits the placeholders say inherits — no invented numbers", () => {
  renderSection({ limits: undefined });
  expect(screen.getByLabelText(/tool_calls/i).getAttribute("placeholder")).toBe(
    "inherits",
  );
});

test("a bound above the ceiling is NAMED, and the form still submits it", () => {
  const onChange = vi.fn();
  renderSection({
    values: { ...emptyCodemodeFieldValues, toolCalls: "5000" },
    onChange,
  });
  const warning = screen.getByRole("status");
  expect(warning.textContent).toContain("above the platform ceiling");
  expect(warning.textContent).toContain("tool_calls");
  // The input stays editable: the clamp owns the bound, the form only warns.
  const input = screen.getByLabelText(/tool_calls/i) as HTMLInputElement;
  expect(input.disabled).toBe(false);
  fireEvent.change(input, { target: { value: "512" } });
  expect(onChange).toHaveBeenCalledWith({ toolCalls: "512" });
});

test("a refused route is named on the section, not left to the picker alone", () => {
  renderSection({
    routeRefusals: { summarise: "this node has codemode: on…" },
  });
  expect(screen.getByText(/node summarise will be refused at dispatch/)).toBeTruthy();
});

test("a memory bound under the floor is NAMED — the run would start above it", () => {
  renderSection({ values: { ...emptyCodemodeFieldValues, memory: "1000" } });
  const status = screen.getByRole("status");
  expect(status.textContent).toContain("below the 67108864-byte floor");
});

test("a typed 0 does not claim to override the bot — nothing travels", () => {
  renderSection({ values: { ...emptyCodemodeFieldValues, toolCalls: "0" } });
  expect(screen.queryByText(/overriding the bot/)).toBeNull();
});

test("a real bound says it overrides the bot", () => {
  renderSection({ values: { ...emptyCodemodeFieldValues, toolCalls: "256" } });
  expect(screen.getByText(/overriding the bot/)).toBeTruthy();
});

test("a numeric field holding a droppable value is marked invalid and says so", () => {
  renderSection({ values: { ...emptyCodemodeFieldValues, toolCalls: "1,000" } });
  const input = screen.getByLabelText(/tool_calls/i) as HTMLInputElement;
  expect(input.getAttribute("aria-invalid")).toBe("true");
  expect(screen.getByText(/not a positive number/)).toBeTruthy();
});
