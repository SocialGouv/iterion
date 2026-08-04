// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";

import {
  DRAWER_WIDTH_DEFAULT,
  DRAWER_WIDTH_KEY,
  DRAWER_WIDTH_MAX,
  DRAWER_WIDTH_MIN,
  clampDrawerWidth,
  drawerWidthBounds,
  readDrawerWidth,
  writeDrawerWidth,
} from "./cardDrawerWidth";

const WIDE = 1920;

describe("drawerWidthBounds", () => {
  it("uses the static band on a wide viewport", () => {
    expect(drawerWidthBounds(WIDE)).toEqual({
      min: DRAWER_WIDTH_MIN,
      max: DRAWER_WIDTH_MAX,
    });
  });

  it("caps the ceiling at the viewport so the handle stays reachable", () => {
    expect(drawerWidthBounds(900)).toEqual({ min: DRAWER_WIDTH_MIN, max: 900 });
  });

  it("lets the floor give way on a viewport narrower than the floor", () => {
    // A 280px phone can't host a 320px drawer; an empty band would be worse.
    expect(drawerWidthBounds(280)).toEqual({ min: 280, max: 280 });
  });

  it("falls back to the static ceiling for a nonsense viewport", () => {
    expect(drawerWidthBounds(0).max).toBe(DRAWER_WIDTH_MAX);
    expect(drawerWidthBounds(Number.NaN).max).toBe(DRAWER_WIDTH_MAX);
  });
});

describe("clampDrawerWidth", () => {
  it("keeps a sane width untouched (rounded)", () => {
    expect(clampDrawerWidth(600.4, WIDE)).toBe(600);
  });

  it("clamps below the floor and above the ceiling", () => {
    expect(clampDrawerWidth(10, WIDE)).toBe(DRAWER_WIDTH_MIN);
    expect(clampDrawerWidth(9999, WIDE)).toBe(DRAWER_WIDTH_MAX);
    expect(clampDrawerWidth(9999, 900)).toBe(900);
  });

  it("falls back to the default for a non-finite width", () => {
    expect(clampDrawerWidth(Number.NaN, WIDE)).toBe(DRAWER_WIDTH_DEFAULT);
    expect(clampDrawerWidth(Number.POSITIVE_INFINITY, WIDE)).toBe(DRAWER_WIDTH_DEFAULT);
  });
});

describe("readDrawerWidth / writeDrawerWidth", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("defaults to the historical fixed width when nothing is stored", () => {
    expect(readDrawerWidth(WIDE)).toBe(DRAWER_WIDTH_DEFAULT);
  });

  it("round-trips a dragged width", () => {
    writeDrawerWidth(742.6);
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe("743");
    expect(readDrawerWidth(WIDE)).toBe(743);
  });

  it("re-clamps a width saved on a wider monitor", () => {
    writeDrawerWidth(1300);
    expect(readDrawerWidth(1024)).toBe(1024);
  });

  it("ignores a corrupted entry", () => {
    window.localStorage.setItem(DRAWER_WIDTH_KEY, "not-a-number");
    expect(readDrawerWidth(WIDE)).toBe(DRAWER_WIDTH_DEFAULT);
  });
});
