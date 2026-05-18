// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import axe from "axe-core";

import { Button } from "@/components/ui/Button";
import { IconButton } from "@/components/ui/IconButton";
import { EmptyState } from "@/components/ui/EmptyState";
import { Spinner } from "@/components/ui/Spinner";
import { LiveDot } from "@/components/ui/LiveDot";
import { Badge } from "@/components/ui/Badge";
import { Skeleton } from "@/components/ui/Skeleton";

// Smoke a11y test for the shared UI primitives. Uses axe-core in
// jsdom and focuses on WCAG 2.1 A + AA rules. The aim is to catch
// regressions on the building blocks — full-page audits stay manual
// (axe browser extension) because the canvas + WebSocket flows are
// out of jsdom's reach.

async function expectNoViolations(container: HTMLElement, label: string) {
  const results = await axe.run(container, {
    runOnly: {
      type: "tag",
      values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"],
    },
  });
  if (results.violations.length > 0) {
    const summary = results.violations
      .map(
        (v) =>
          `  - [${v.id}] ${v.help} (${v.nodes.length} node${v.nodes.length > 1 ? "s" : ""})`,
      )
      .join("\n");
    throw new Error(`${label} has axe violations:\n${summary}`);
  }
  expect(results.violations).toHaveLength(0);
}

function mount(node: ReactNode): HTMLElement {
  return render(node).container;
}

describe("a11y / primitives", () => {
  afterEach(() => {
    cleanup();
  });

  it("Button — all variants × all sizes", async () => {
    const root = mount(
      <div>
        <Button variant="primary" size="md">Save</Button>
        <Button variant="secondary" size="sm">Cancel</Button>
        <Button variant="ghost" size="sm">Skip</Button>
        <Button variant="danger" size="md">Delete</Button>
        <Button variant="primary" size="md" disabled>Disabled</Button>
        <Button variant="primary" size="md" loading>Loading</Button>
      </div>,
    );
    await expectNoViolations(root, "Button");
  });

  it("IconButton requires a label and exposes aria-label", async () => {
    const root = mount(
      <div>
        <IconButton label="Refresh">↻</IconButton>
        <IconButton label="Close" variant="ghost">✕</IconButton>
        <IconButton label="Delete" variant="danger" disabled>🗑</IconButton>
      </div>,
    );
    await expectNoViolations(root, "IconButton");
  });

  it("EmptyState", async () => {
    const root = mount(
      <main>
        <EmptyState message="No runs yet" />
      </main>,
    );
    await expectNoViolations(root, "EmptyState");
  });

  it("Spinner with screen-reader label", async () => {
    const root = mount(
      <div>
        <Spinner size="sm" label="Loading data" />
      </div>,
    );
    await expectNoViolations(root, "Spinner");
  });

  it("LiveDot — every tone", async () => {
    const root = mount(
      <div>
        <LiveDot tone="info" label="Run active" />
        <LiveDot tone="success" label="Connected" />
        <LiveDot tone="warning" label="Reconnecting" />
        <LiveDot tone="danger" pulse={false} label="Disconnected" />
        <LiveDot tone="neutral" pulse={false} label="Unknown" />
      </div>,
    );
    await expectNoViolations(root, "LiveDot");
  });

  it("Badge variants", async () => {
    const root = mount(
      <div>
        <Badge variant="neutral">queued</Badge>
        <Badge variant="info">running</Badge>
        <Badge variant="success">finished</Badge>
        <Badge variant="warning">paused</Badge>
        <Badge variant="danger">failed</Badge>
      </div>,
    );
    await expectNoViolations(root, "Badge");
  });

  it("Skeleton is aria-hidden", async () => {
    const root = mount(<Skeleton className="h-6 w-32" />);
    await expectNoViolations(root, "Skeleton");
  });
});
