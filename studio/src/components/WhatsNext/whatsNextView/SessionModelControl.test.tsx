// @vitest-environment jsdom

import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { UseSessionModelPrefResult } from "@/hooks/useSessionModelPref";

import SessionModelControl from "./SessionModelControl";

vi.mock("@/hooks/useModelCatalog", () => ({
  useModelCatalog: () => ({
    models: [
      {
        spec: "openai/unproven",
        provider: "openai",
        model: "unproven",
        backends: [],
        usable: false,
        reachability: "unknown",
      },
    ],
    recommended: null,
    resolvedDefaultBackend: "",
    error: null,
  }),
}));
vi.mock("@/components/models/ModelPicker", () => ({
  default: ({ onChange }: { onChange: (spec: string) => void }) => (
    <button type="button" onClick={() => onChange("openai/unproven")}>
      choose unproven
    </button>
  ),
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

  it("clears the previous backend when the resolved model drives none", async () => {
    const save = vi.fn();
    render(
      <SessionModelControl
        pref={pref({
          choice: { model: "anthropic/old", backend: "claude_code" },
          save,
        })}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /model:/i }));
    fireEvent.click(screen.getByRole("button", { name: "choose unproven" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(save).toHaveBeenCalledWith({
        model: "openai/unproven",
        backend: "",
      }),
    );
  });
});
