// @vitest-environment jsdom

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import RunSettingsSection from "./RunSettingsSection";

afterEach(cleanup);

test("shows that a run permission override wins a node permission pin", () => {
  const { container } = render(
    <RunSettingsSection
      backendOverride=""
      compressOverride=""
      autoMemoryOverride=""
      permissionOverride="deny"
      reviewModeOverride=""
      backendReport={null}
      effective={{
        backend: { effective: "auto", source: "default" },
        compress: { effective: "auto", source: "default" },
        auto_memory: { effective: "off", source: "default" },
        permission: {
          effective: "ask",
          source: "workflow",
          node_pinned: true,
        },
      }}
      onBackendChange={vi.fn()}
      onCompressChange={vi.fn()}
      onAutoMemoryChange={vi.fn()}
      onPermissionChange={vi.fn()}
      onReviewModeChange={vi.fn()}
      showReviewMode={false}
    />,
  );

  expect(container.textContent).toContain(
    "effective: deny · from run override · run override wins over node settings",
  );
  expect(container.textContent).not.toContain("override won’t affect them");
});
