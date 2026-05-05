import { describe, expect, it, vi } from "vitest";

import { selectOnboardingProject } from "./ProjectPicker";

describe("selectOnboardingProject", () => {
  it("uses the silent add path and does not trigger a reload during onboarding", async () => {
    const reload = vi.fn();
    const bridge = {
      pickProjectDirectory: vi.fn().mockResolvedValue("/tmp/p"),
      scaffoldProject: vi.fn().mockResolvedValue(undefined),
      addProjectSilently: vi.fn().mockResolvedValue({ id: "p" }),
      addProject: vi.fn().mockResolvedValue({ id: "p" }),
    };

    const selected = await selectOnboardingProject(bridge, false);

    expect(selected).toBe(true);
    expect(bridge.addProjectSilently).toHaveBeenCalledWith("/tmp/p");
    expect(bridge.addProject).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });

  it("scaffolds before silently adding when creating a new project", async () => {
    const calls: string[] = [];
    const bridge = {
      pickProjectDirectory: vi.fn().mockResolvedValue("/tmp/new"),
      scaffoldProject: vi.fn().mockImplementation(async () => { calls.push("scaffold"); }),
      addProjectSilently: vi.fn().mockImplementation(async () => { calls.push("add-silent"); }),
    };

    await selectOnboardingProject(bridge, true);

    expect(calls).toEqual(["scaffold", "add-silent"]);
  });
});
