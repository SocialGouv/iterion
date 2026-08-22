// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { UseSessionModelPrefResult } from "@/hooks/useSessionModelPref";

import SessionModelControl from "./SessionModelControl";

vi.mock("@/hooks/useModelCatalog", () => ({
  useModelCatalog: () => ({
    models: [],
    recommended: null,
    resolvedDefaultBackend: "",
    error: null,
  }),
}));
vi.mock("@/components/models/ModelPicker", () => ({
  default: () => <div data-testid="model-picker" />,
}));

afterEach(cleanup);

function pref(
  patch: Partial<UseSessionModelPrefResult> = {},
): UseSessionModelPrefResult {
  return {
    choice: {},
    set: false,
    loading: false,
    saving: false,
    error: null,
    available: true,
    save: vi.fn(),
    reset: vi.fn(),
    current: () => ({}),
    ...patch,
  };
}

describe("SessionModelControl preference errors", () => {
  it("shows the real persistence error instead of the unavailable-store copy", () => {
    render(
      <SessionModelControl
        pref={pref({ available: false, error: "unknown backend claw-x" })}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /model:/i }));

    expect(screen.getByText("unknown backend claw-x")).toBeTruthy();
    expect(screen.queryByText(/cannot remember the choice/i)).toBeNull();
  });

  it("keeps the local-only explanation for an unavailable store without detail", () => {
    render(<SessionModelControl pref={pref({ available: false })} />);
    fireEvent.click(screen.getByRole("button", { name: /model:/i }));

    expect(screen.getByText(/cannot remember the choice/i)).toBeTruthy();
  });
});
