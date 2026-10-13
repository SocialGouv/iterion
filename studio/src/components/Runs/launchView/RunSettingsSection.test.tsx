// @vitest-environment jsdom

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { PERMISSION_OPTIONS } from "@/lib/dslOptions";

import RunSettingsSection from "./RunSettingsSection";

afterEach(cleanup);

/** The select of one launch knob, found by the row's own name rather than by
 *  any option text — the option texts are what the assertions read. */
function knobSelect(container: HTMLElement, knob: string): HTMLSelectElement {
  const name = Array.from(container.querySelectorAll("div.font-mono")).find(
    (d) => d.textContent?.trim() === knob,
  );
  if (!name) throw new Error(`no launch row named ${knob}`);
  const select = name.closest("div.grid")?.querySelector("select");
  if (!select) throw new Error(`the launch row ${knob} has no select`);
  return select as HTMLSelectElement;
}

function launchSettings() {
  return render(
    <RunSettingsSection
      backendOverride=""
      compressOverride=""
      autoMemoryOverride=""
      ambientContextOverride=""
      permissionOverride=""
      reviewModeOverride=""
      backendReport={null}
      effective={{
        backend: { effective: "auto", source: "default" },
        compress: { effective: "auto", source: "default" },
        auto_memory: { effective: "off", source: "default" },
        ambient_context: { effective: "workspace", source: "default" },
        permission: { effective: "off", source: "default" },
      }}
      onBackendChange={vi.fn()}
      onCompressChange={vi.fn()}
      onAutoMemoryChange={vi.fn()}
      onAmbientContextChange={vi.fn()}
      onPermissionChange={vi.fn()}
      onReviewModeChange={vi.fn()}
      showReviewMode={false}
    />,
  );
}

// This select is the THIRD surface that offers the gate's mode words to a
// human, and the one that does not read PERMISSION_OPTIONS: at launch, empty
// means "inherit" rather than "unset" and `off` is the resolved default, so
// its labels and their order are its own. The VALUES are not: a mode the DSL
// accepts and this select omits cannot be overridden at launch, and a mode
// offered here that the DSL does not accept is refused after the operator
// picked it. Compared as sets, since the order differs on purpose.
test("the launch override offers exactly the DSL's gate modes, plus inherit", () => {
  const { container } = launchSettings();
  const got = Array.from(knobSelect(container, "permission").options).map((o) => o.value);
  const want = ["", ...PERMISSION_OPTIONS.map((o) => o.value)];
  expect([...got].sort()).toEqual([...want].sort());
});

test("shows that a run permission override wins a node permission pin", () => {
  const { container } = render(
    <RunSettingsSection
      backendOverride=""
      compressOverride=""
      autoMemoryOverride=""
      ambientContextOverride=""
      permissionOverride="deny"
      reviewModeOverride=""
      backendReport={null}
      effective={{
        backend: { effective: "auto", source: "default" },
        compress: { effective: "auto", source: "default" },
        auto_memory: { effective: "off", source: "default" },
        ambient_context: { effective: "workspace", source: "default" },
        permission: {
          effective: "ask",
          source: "workflow",
          node_pinned: true,
        },
      }}
      onBackendChange={vi.fn()}
      onCompressChange={vi.fn()}
      onAutoMemoryChange={vi.fn()}
      onAmbientContextChange={vi.fn()}
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

test("shows that the ambient-context default is the repository, not the operator", () => {
  const { container } = render(
    <RunSettingsSection
      backendOverride=""
      compressOverride=""
      autoMemoryOverride=""
      ambientContextOverride=""
      permissionOverride=""
      reviewModeOverride=""
      backendReport={null}
      effective={{
        backend: { effective: "auto", source: "default" },
        compress: { effective: "auto", source: "default" },
        auto_memory: { effective: "off", source: "default" },
        ambient_context: { effective: "workspace", source: "default" },
        permission: { effective: "off", source: "default" },
      }}
      onBackendChange={vi.fn()}
      onCompressChange={vi.fn()}
      onAutoMemoryChange={vi.fn()}
      onAmbientContextChange={vi.fn()}
      onPermissionChange={vi.fn()}
      onReviewModeChange={vi.fn()}
      showReviewMode={false}
    />,
  );
  expect(container.textContent).toContain(
    "effective: workspace · from default",
  );
  expect(container.textContent).toContain(
    "A chosen value a node's backend cannot apply refuses that node",
  );
});
