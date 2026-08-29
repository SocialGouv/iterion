// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import type { BackendDetectReport } from "@/api/backends";

import ModelOverridesSection from "./ModelOverridesSection";

afterEach(cleanup);

vi.mock("@/hooks/useModelCatalog", () => ({
  useModelCatalog: () => ({
    models: [],
    recommended: null,
    invalidSpecs: [],
    error: null,
  }),
}));

vi.mock("@/components/models/ModelPicker", () => ({
  default: () => <input aria-label="model override" />,
}));

const report: BackendDetectReport = {
  preference_order: ["claude_code", "claw"],
  resolved_default: "claw",
  providers: [],
  backends: [
    { name: "claw", available: true, auth: "api_key" },
    { name: "claude_code", available: true, auth: "oauth" },
    { name: "codex", available: true, auth: "oauth" },
  ],
};

const gatedNode = {
  name: "gated",
  kind: "agent" as const,
  model: "a/model",
  backend: "claw",
};

const nodes = [
  gatedNode,
  { name: "open", kind: "agent" as const, model: "a/model", backend: "claw" },
];

function openPicker() {
  fireEvent.click(screen.getByRole("button", { name: /Model & backend per node/i }));
}

test("disables an unsafe backend only for the gated node and explains why", () => {
  render(
    <ModelOverridesSection
      nodes={nodes}
      overrides={{}}
      backendReport={report}
      backendOptions={{
        gated: {
          codex: {
            unavailable_reason:
              'runs on backend "codex", which cannot enforce the effective permission: deny gate — the run would be UNGATED',
          },
        },
        open: { codex: {} },
      }}
      backendOptionsReady
      backendOptionsError={false}
      onChange={vi.fn()}
    />,
  );
  openPicker();

  const gated = screen.getByLabelText("Backend override for gated");
  const open = screen.getByLabelText("Backend override for open");
  const gatedCodex = within(gated).getByRole("option", { name: /codex/ });
  const openCodex = within(open).getByRole("option", { name: /codex/ });

  expect((gatedCodex as HTMLOptionElement).disabled).toBe(true);
  expect(gatedCodex.textContent).toContain("cannot enforce permission: deny");
  expect((openCodex as HTMLOptionElement).disabled).toBe(false);
  expect(
    screen.getByText(/cannot preserve this node's effective permission gate/i),
  ).toBeTruthy();
});

test("warns when a claw tools restriction becomes inert on a CLI backend", () => {
  render(
    <ModelOverridesSection
      nodes={[gatedNode]}
      overrides={{ gated: { backend: "claude_code" } }}
      backendReport={report}
      backendOptions={{
        gated: {
          claude_code: {
            warning:
              "this claw node's tools: restriction is not enforced by the selected CLI backend; the permission gate remains active",
          },
        },
      }}
      backendOptionsReady
      backendOptionsError={false}
      onChange={vi.fn()}
    />,
  );
  openPicker();

  expect(screen.getByRole("status").textContent).toContain(
    "tools: restriction is not enforced",
  );
  expect(screen.getByRole("status").textContent).toContain(
    "permission gate remains active",
  );
});

test("clears a stale selection that becomes unsafe after a run gate change", () => {
  const onChange = vi.fn();
  render(
    <ModelOverridesSection
      nodes={[gatedNode]}
      overrides={{ gated: { backend: "codex" } }}
      backendReport={report}
      backendOptions={{
        gated: {
          codex: { unavailable_reason: "cannot preserve the permission gate" },
        },
      }}
      backendOptionsReady
      backendOptionsError={false}
      onChange={onChange}
    />,
  );

  expect(onChange).toHaveBeenCalledWith("gated", { backend: "" });
});
