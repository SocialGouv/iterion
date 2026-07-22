import { describe, expect, it } from "vitest";

import {
  SOURCE_KIND_ORDER,
  SOURCE_META,
  metaForSource,
  normalizeSourceKind,
  runSourceKind,
} from "./runSourceMeta";

describe("normalizeSourceKind", () => {
  it("passes every known kind through", () => {
    for (const kind of SOURCE_KIND_ORDER) {
      expect(normalizeSourceKind(kind)).toBe(kind);
    }
  });

  // Regression: cron-fired runs were rendered "Manual" because the studio
  // didn't know the backend's "schedule" kind and normalised it away.
  it("keeps schedule distinct from manual", () => {
    expect(normalizeSourceKind("schedule")).toBe("schedule");
    expect(runSourceKind({ source_kind: "schedule" })).toBe("schedule");
    expect(metaForSource("schedule").label).toBe("Schedule");
  });

  it("falls back to manual for empty and unknown values", () => {
    expect(normalizeSourceKind(undefined)).toBe("manual");
    expect(normalizeSourceKind("")).toBe("manual");
    expect(normalizeSourceKind("from-the-future")).toBe("manual");
  });
});

describe("SOURCE_META", () => {
  it("covers every ordered kind", () => {
    for (const kind of SOURCE_KIND_ORDER) {
      expect(SOURCE_META[kind]).toBeDefined();
    }
    expect(Object.keys(SOURCE_META).sort()).toEqual(
      [...SOURCE_KIND_ORDER].sort(),
    );
  });
});
