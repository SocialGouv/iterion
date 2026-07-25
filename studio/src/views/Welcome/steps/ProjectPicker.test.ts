import { describe, expect, it, vi } from "vitest";

import { selectOnboardingProject } from "./ProjectPicker";

describe("selectOnboardingProject", () => {
  it("uses the silent add path and does not trigger a reload during onboarding", async () => {
    const reload = vi.fn();
    const bridge = {
      pickProjectDirectory: vi.fn().mockResolvedValue("/tmp/p"),
      addProjectSilently: vi.fn().mockResolvedValue({ id: "p" }),
      addProject: vi.fn().mockResolvedValue({ id: "p" }),
    };

    const selected = await selectOnboardingProject(bridge);

    expect(selected).toBe(true);
    expect(bridge.addProjectSilently).toHaveBeenCalledWith("/tmp/p");
    expect(bridge.addProject).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });

  it("registers an empty folder as-is, writing nothing into it", async () => {
    // Onboarding no longer scaffolds a workflow: an empty folder is a
    // valid project, and what to put in it is the operator's choice.
    const bridge = {
      pickProjectDirectory: vi.fn().mockResolvedValue("/tmp/empty"),
      addProjectSilently: vi.fn().mockResolvedValue({ id: "p" }),
    };

    const selected = await selectOnboardingProject(bridge);

    expect(selected).toBe(true);
    expect(bridge.addProjectSilently).toHaveBeenCalledWith("/tmp/empty");
  });

  it("returns false and adds nothing when the picker is cancelled", async () => {
    const bridge = {
      pickProjectDirectory: vi.fn().mockResolvedValue(""),
      addProjectSilently: vi.fn().mockResolvedValue({ id: "p" }),
    };

    const selected = await selectOnboardingProject(bridge);

    expect(selected).toBe(false);
    expect(bridge.addProjectSilently).not.toHaveBeenCalled();
  });
});
