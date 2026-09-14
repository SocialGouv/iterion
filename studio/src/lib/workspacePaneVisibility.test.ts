// @vitest-environment jsdom

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  isWorkspacePaneVisibilityMessage,
  workspacePaneStartsVisible,
  useWorkspacePaneVisibility,
  WORKSPACE_PANE_VISIBILITY,
  WORKSPACE_PANE_VISIBILITY_REQUEST,
} from "./workspacePaneVisibility";

const standaloneParent = window.parent;

afterEach(() => {
  cleanup();
  delete (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__;
  Object.defineProperty(window, "parent", {
    configurable: true,
    value: standaloneParent,
  });
});

describe("workspace pane visibility", () => {
  it("defaults a standalone scoped page to visible", () => {
    const standalone = {} as Window;
    Object.assign(standalone, { parent: standalone });
    expect(workspacePaneStartsVisible("project-a", standalone)).toBe(true);
    expect(workspacePaneStartsVisible(null, { parent: {} as Window })).toBe(true);
  });

  it("keeps an embedded pane hidden until its exact shell message arrives", () => {
    const parent = {} as Window;
    expect(workspacePaneStartsVisible("project-a", { parent })).toBe(false);
    const valid = {
      origin: "http://iterion.test",
      source: parent,
      data: {
        source: "iterion-shell",
        type: WORKSPACE_PANE_VISIBILITY,
        projectId: "project-a",
        visible: true,
      },
    } as unknown as MessageEvent;
    expect(
      isWorkspacePaneVisibilityMessage(
        valid,
        parent,
        "http://iterion.test",
        "project-a",
      ),
    ).toBe(true);
    expect(
      isWorkspacePaneVisibilityMessage(
        { ...valid, origin: "http://foreign.test" } as MessageEvent,
        parent,
        "http://iterion.test",
        "project-a",
      ),
    ).toBe(false);
    expect(
      isWorkspacePaneVisibilityMessage(
        {
          ...valid,
          data: { ...valid.data, projectId: "project-b" },
        } as MessageEvent,
        parent,
        "http://iterion.test",
        "project-a",
      ),
    ).toBe(false);
  });

  it("updates the hook only after the owning shell marks the pane visible", () => {
    const parent = { postMessage: vi.fn() } as unknown as Window;
    Object.defineProperty(window, "parent", {
      configurable: true,
      value: parent,
    });
    (globalThis as { __ITERION_SCOPE__?: string }).__ITERION_SCOPE__ =
      "/x/project-a";

    const { result } = renderHook(() => useWorkspacePaneVisibility());
    expect(result.current).toBe(false);
    expect(parent.postMessage).toHaveBeenCalledWith(
      {
        source: "iterion-pane",
        type: WORKSPACE_PANE_VISIBILITY_REQUEST,
        projectId: "project-a",
      },
      window.location.origin,
    );

    act(() => {
      window.dispatchEvent(
        new MessageEvent("message", {
          origin: window.location.origin,
          source: parent,
          data: {
            source: "iterion-shell",
            type: WORKSPACE_PANE_VISIBILITY,
            projectId: "project-a",
            visible: true,
          },
        }),
      );
    });
    expect(result.current).toBe(true);
  });
});
