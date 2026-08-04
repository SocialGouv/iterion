// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import {
  COLLAPSE_MAX_CHARS,
  COLLAPSE_MAX_LINES,
  ExpandableValue,
  countLines,
  isLongValue,
  valueRepresentations,
} from "./ExpandableValue";

afterEach(cleanup);

describe("valueRepresentations", () => {
  it("leaves plain scalars alone — no raw form, so no toggle", () => {
    expect(valueRepresentations("jazz")).toEqual({ pretty: "jazz", raw: null });
    expect(valueRepresentations(120)).toEqual({ pretty: "120", raw: null });
    expect(valueRepresentations(true)).toEqual({ pretty: "true", raw: null });
    expect(valueRepresentations(null)).toEqual({ pretty: "", raw: null });
    expect(valueRepresentations(undefined)).toEqual({ pretty: "", raw: null });
  });

  it("pretty-prints a structured value and keeps the compact form as raw", () => {
    const { pretty, raw } = valueRepresentations({ tempo: 120, mood: "calm" });
    expect(pretty).toContain('"tempo": 120');
    expect(raw).toBe('{"tempo":120,"mood":"calm"}');
  });

  it("pretty-prints a JSON *string* and keeps the verbatim text as raw", () => {
    const { pretty, raw } = valueRepresentations('{"tempo":120}');
    expect(pretty).toBe('{\n  "tempo": 120\n}');
    expect(raw).toBe('{"tempo":120}');
  });

  it("does not offer a raw form when the string is already pretty-printed", () => {
    const already = '{\n  "tempo": 120\n}';
    expect(valueRepresentations(already)).toEqual({ pretty: already, raw: null });
  });

  it("a sentence that merely starts with a brace stays plain text", () => {
    const text = "{not json at all";
    expect(valueRepresentations(text)).toEqual({ pretty: text, raw: null });
  });

  it("a circular value degrades to a string instead of throwing", () => {
    const circular: Record<string, unknown> = {};
    circular.self = circular;
    expect(() => valueRepresentations(circular)).not.toThrow();
    expect(valueRepresentations(circular).raw).toBeNull();
  });
});

describe("isLongValue", () => {
  it("is false for a short value", () => {
    expect(isLongValue("jazz")).toBe(false);
    expect(countLines("")).toBe(0);
    expect(countLines("a\nb")).toBe(2);
  });

  it("is true past the char bound (one giant unwrapped paragraph)", () => {
    expect(isLongValue("x".repeat(COLLAPSE_MAX_CHARS + 1))).toBe(true);
    expect(isLongValue("x".repeat(COLLAPSE_MAX_CHARS))).toBe(false);
  });

  it("is true past the line bound (tall but narrow)", () => {
    const tall = Array.from({ length: COLLAPSE_MAX_LINES + 1 }, () => "a").join("\n");
    expect(isLongValue(tall)).toBe(true);
    const justUnder = Array.from({ length: COLLAPSE_MAX_LINES }, () => "a").join("\n");
    expect(isLongValue(justUnder)).toBe(false);
  });
});

const LONG = Array.from({ length: 40 }, (_, i) => `line ${i}`).join("\n");

describe("<ExpandableValue />", () => {
  it("renders a short value whole, with no expand toggle", () => {
    render(<ExpandableValue value="jazz" label="topic" />);
    expect(screen.getByText("jazz")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /show less/i })).toBeNull();
  });

  it("collapses a long value and expands it in place — nothing is truncated away", () => {
    const { container } = render(<ExpandableValue value={LONG} label="brief" />);
    const pre = container.querySelector("pre");
    expect(pre).toBeTruthy();
    // The full text is always in the DOM; only the visible height is capped.
    expect(pre?.textContent).toBe(LONG);
    expect(pre?.style.maxHeight).toBe("12rem");

    const toggle = screen.getByRole("button", { name: "Show all 40 lines" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // aria-controls points at the block it expands.
    expect(toggle.getAttribute("aria-controls")).toBe(pre?.id);

    fireEvent.click(toggle);
    expect(pre?.style.maxHeight).toBe("");
    const collapse = screen.getByRole("button", { name: "Show less" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");

    fireEvent.click(collapse);
    expect(container.querySelector("pre")?.style.maxHeight).toBe("12rem");
  });

  it("honours a caller-supplied collapsed height and defaultExpanded", () => {
    const { container } = render(
      <ExpandableValue value={LONG} collapsedMaxHeight="30rem" defaultExpanded />,
    );
    expect(container.querySelector("pre")?.style.maxHeight).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Show less" }));
    expect(container.querySelector("pre")?.style.maxHeight).toBe("30rem");
  });

  it("toggles a JSON value between pretty and raw", () => {
    const { container } = render(
      <ExpandableValue value={{ tempo: 120, mood: "calm" }} label="options" />,
    );
    expect(container.querySelector("pre")?.textContent).toContain('"tempo": 120');

    fireEvent.click(screen.getByRole("button", { name: "Show raw options" }));
    expect(container.querySelector("pre")?.textContent).toBe(
      '{"tempo":120,"mood":"calm"}',
    );

    fireEvent.click(screen.getByRole("button", { name: "Show pretty-printed options" }));
    expect(container.querySelector("pre")?.textContent).toContain('"tempo": 120');
  });

  it("offers no raw toggle for a value that has only one form", () => {
    render(<ExpandableValue value="jazz" label="topic" />);
    expect(screen.queryByRole("button", { name: /raw/i })).toBeNull();
  });

  function stubClipboard() {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    return writeText;
  }

  it("copies the value to the clipboard", async () => {
    const writeText = stubClipboard();
    render(<ExpandableValue value={{ tempo: 120 }} label="options" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy options" }));
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('{\n  "tempo": 120\n}'));
  });

  it("copies the CURRENTLY SHOWN form, not always the pretty one", async () => {
    const writeText = stubClipboard();
    render(<ExpandableValue value={{ tempo: 120 }} label="options" />);
    fireEvent.click(screen.getByRole("button", { name: "Show raw options" }));
    fireEvent.click(screen.getByRole("button", { name: "Copy options" }));
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('{"tempo":120}'));
  });

  it("renders as a <dd> when the caller is inside a <dl>", () => {
    const { container } = render(
      <dl>
        <dt>topic</dt>
        <ExpandableValue value="jazz" as="dd" label="topic" />
      </dl>,
    );
    expect(container.querySelector("dd")).toBeTruthy();
  });
});
