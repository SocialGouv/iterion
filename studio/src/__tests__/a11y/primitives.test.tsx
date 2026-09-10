// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";

import { BrandMark } from "@/components/ui/BrandMark";
import { Button } from "@/components/ui/Button";
import { IconButton } from "@/components/ui/IconButton";
import { EmptyState } from "@/components/ui/EmptyState";
import { DesktopOnlyNotice } from "@/components/ui/DesktopOnlyNotice";
import { Spinner } from "@/components/ui/Spinner";
import { LiveDot } from "@/components/ui/LiveDot";
import { Stat } from "@/components/ui/Stat";
import { Meter } from "@/components/ui/Meter";
import { Badge } from "@/components/ui/Badge";
import { Skeleton } from "@/components/ui/Skeleton";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Checkbox } from "@/components/ui/Checkbox";
import { Radio } from "@/components/ui/Radio";
import { RadioGroup } from "@/components/ui/RadioGroup";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { BrandWordmark } from "@/components/ui/BrandWordmark";
import { TerminalCaret } from "@/components/ui/TerminalCaret";
import CommandPalette from "@/components/shared/CommandPalette";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Textarea } from "@/components/ui/Textarea";
import { Drawer } from "@/components/ui/Drawer";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { setupMatchMedia, expectNoViolations } from "./axeHelpers";

// Smoke a11y test for the shared UI primitives. Uses axe-core in
// jsdom and focuses on WCAG 2.1 A + AA rules. The aim is to catch
// regressions on the building blocks — full-page audits stay manual
// (axe browser extension) because the canvas + WebSocket flows are
// out of jsdom's reach.

// jsdom lacks matchMedia (DesktopOnlyNotice + theme store read it on mount).
setupMatchMedia();

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
        <Button variant="success" size="md">Go</Button>
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
        <LiveDot tone="info" label="Informational" />
        <LiveDot tone="live" label="Run active" />
        <LiveDot tone="success" label="Connected" />
        <LiveDot tone="warning" label="Reconnecting" />
        <LiveDot tone="danger" pulse={false} label="Disconnected" />
        <LiveDot tone="neutral" pulse={false} label="Unknown" />
      </div>,
    );
    await expectNoViolations(root, "LiveDot");
  });

  it("Stat — every tone, sizes, live, stack, button", async () => {
    const root = mount(
      <div>
        <Stat label="cost" value="$1.20" />
        <Stat label="tokens" value="42k" tone="info" size="md" />
        <Stat label="paused" value="2" tone="warning" />
        <Stat label="failed" value="1" tone="danger" onClick={() => {}} />
        <Stat label="nodes" value="12" tone="success" size="lg" align="stack" />
        <Stat label="duration" value="1m 03s" tone="live" live hint="active" />
      </div>,
    );
    await expectNoViolations(root, "Stat");
  });

  it("Meter — sizes, tones, no-cap, compact, full", async () => {
    const root = mount(
      <div>
        <Meter label="Cost" value={1.2} max={5} formatValue={(v) => `$${v}`} />
        <Meter label="Tokens" value={180000} max={200000} size="md" />
        <Meter label="Duration" value={9} max={10} fixedTone="danger" />
        <Meter label="Iterations" value={3} hint="no cap set" />
        <Meter value={40} max={100} compact size="xs" hint="context window" />
        <Meter label="Review loop" value={3} max={5} compact fixedTone="live" />
      </div>,
    );
    await expectNoViolations(root, "Meter");
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

  it("InlineBanner — every tone × layout, with action + dismiss", async () => {
    const tones = ["info", "warning", "danger", "success"] as const;
    const root = mount(
      <main>
        {tones.map((tone) => (
          <div key={tone}>
            <InlineBanner tone={tone} layout="sticky" title="Heading">
              Body copy for the {tone} banner.
            </InlineBanner>
            <InlineBanner
              tone={tone}
              layout="inline"
              suffix="(github)"
              action={
                <Button variant="ghost" size="sm">
                  Retry
                </Button>
              }
            >
              Inline {tone} message.
            </InlineBanner>
            <InlineBanner tone={tone} dismissable onDismiss={() => {}}>
              Dismissable {tone}.
            </InlineBanner>
          </div>
        ))}
      </main>,
    );
    await expectNoViolations(root, "InlineBanner");
  });

  it("Button loading state still passes axe (no orphaned spinner label)", async () => {
    const root = mount(
      <div>
        <Button variant="primary" loading>Launch</Button>
        <Button variant="primary" size="sm" loading>Resume</Button>
      </div>,
    );
    await expectNoViolations(root, "Button loading");
  });

  it("Stale-WS banner — role=status + reconnect Button", async () => {
    // Mirrors RunHeader's WSDisconnectBanner composition shape so axe
    // catches role + nested-button conflicts before the live SPA does.
    const root = mount(
      <main>
        <div role="status" aria-live="polite" className="flex items-center gap-2">
          <LiveDot tone="danger" size="sm" pulse={false} label="Disconnected" />
          <span>Live updates disconnected — data may be stale.</span>
          <Button variant="ghost" size="sm">Reconnect</Button>
        </div>
      </main>,
    );
    await expectNoViolations(root, "WS banner");
  });

  it("DesktopOnlyNotice — desktop branch + narrow notice", async () => {
    // jsdom reports a desktop viewport by default, so the children
    // branch is what the smoke test exercises here. The narrow branch
    // is shape-tested via the manual mobile sweep called out in
    // design-system.md § Responsive scope.
    const root = mount(
      <main>
        <DesktopOnlyNotice feature="the editor">
          <div>desktop UI</div>
        </DesktopOnlyNotice>
      </main>,
    );
    await expectNoViolations(root, "DesktopOnlyNotice");
  });

  it("Toast region — status + alert roles per level", async () => {
    const root = mount(
      <main>
        <div role="region" aria-label="Notifications">
          <div role="status" aria-live="polite">Saved</div>
          <div role="status" aria-live="polite">Reconnecting</div>
          <div role="alert" aria-live="assertive">Save failed</div>
        </div>
      </main>,
    );
    await expectNoViolations(root, "Toast region");
  });

  it("Checkbox — labelled, help, standalone, disabled", async () => {
    const root = mount(
      <main>
        <Checkbox label="Post to board" defaultChecked />
        <Checkbox label="Dry run" help="Skips side effects" />
        <Checkbox aria-label="Select row" />
        <Checkbox label="Disabled option" disabled />
      </main>,
    );
    await expectNoViolations(root, "Checkbox");
  });

  it("Radio + RadioGroup — labelled set", async () => {
    const root = mount(
      <main>
        <RadioGroup
          name="mode"
          label="Connection mode"
          value="app"
          onChange={() => {}}
          options={[
            { value: "app", label: "OAuth app" },
            { value: "pat", label: "Personal token" },
            { value: "off", label: "Disabled", disabled: true },
          ]}
        />
        <Radio name="solo" aria-label="Standalone radio" />
      </main>,
    );
    await expectNoViolations(root, "Radio/RadioGroup");
  });

  it("FieldLabel — associates with its control via htmlFor", async () => {
    const root = mount(
      <main>
        <FieldLabel htmlFor="fld" help="What this field does" helpId="fld-help">
          Run name
        </FieldLabel>
        <input id="fld" type="text" aria-describedby="fld-help" />
      </main>,
    );
    await expectNoViolations(root, "FieldLabel");
  });

  it("BrandMark — the mascot image carries the brand name", async () => {
    const root = mount(<BrandMark className="h-7 w-7" />);
    expect(root.querySelector("img")?.getAttribute("alt")).toBe("Iterion");
    await expectNoViolations(root, "BrandMark");
  });

  it("BrandWordmark — full + compact have an accessible name", async () => {
    const root = mount(
      <div>
        <BrandWordmark />
        <BrandWordmark compact />
      </div>,
    );
    await expectNoViolations(root, "BrandWordmark");
  });

  it("TerminalCaret + EmptyState caret stay decorative (aria-hidden)", async () => {
    const root = mount(
      <main>
        <TerminalCaret />
        <EmptyState message="No runs yet" caret />
      </main>,
    );
    await expectNoViolations(root, "TerminalCaret");
  });

  it("Table — caption, header scope, densities, skeleton", async () => {
    const root = mount(
      <main>
        <Table caption="Personal access tokens">
          <THead>
            <Th>Name</Th>
            <Th>Created</Th>
            <Th align="right">Actions</Th>
          </THead>
          <TBody>
            <Tr>
              <Td>CI bot</Td>
              <Td>2026-07-10</Td>
              <Td align="right">
                <Button variant="ghost" size="sm">Revoke</Button>
              </Td>
            </Tr>
          </TBody>
        </Table>
        <Table caption="Dense dashboard" density="sm" captionVisible>
          <THead>
            <Th>Run</Th>
            <Th>State</Th>
          </THead>
          <TBody>
            <Tr className="bg-warning-soft">
              <Td>abc123</Td>
              <Td>running</Td>
            </Tr>
          </TBody>
        </Table>
        <TableSkeleton rows={2} cols={3} />
      </main>,
    );
    // RGAA 5.4/5.5: the caption element must exist even when visually hidden.
    expect(root.querySelectorAll("caption")).toHaveLength(2);
    // RGAA 5.7: every header cell carries an explicit scope.
    for (const th of root.querySelectorAll("th")) {
      expect(th.getAttribute("scope")).toBe("col");
    }
    await expectNoViolations(root, "Table");
  });

  it("Drawer — Radix sheet with title/description", async () => {
    const root = mount(
      <Drawer open onOpenChange={() => {}} title="Bundle detail" description="Marketplace listing">
        <p>Body</p>
      </Drawer>,
    );
    // Radix portals the content to document.body — audit the whole document.
    await expectNoViolations(document.body, "Drawer");
    void root;
  });

  it("Input/Select/Textarea expose aria-invalid when error is set (RGAA 11.10)", async () => {
    const root = mount(
      <main>
        <Input aria-label="Name" error />
        <Input aria-label="Name ok" />
        <Select aria-label="Team" error>
          <option>a</option>
        </Select>
        <Textarea aria-label="Notes" error />
      </main>,
    );
    expect(root.querySelector("input[aria-label='Name']")?.getAttribute("aria-invalid")).toBe("true");
    expect(root.querySelector("input[aria-label='Name ok']")?.hasAttribute("aria-invalid")).toBe(false);
    expect(root.querySelector("select")?.getAttribute("aria-invalid")).toBe("true");
    expect(root.querySelector("textarea")?.getAttribute("aria-invalid")).toBe("true");
    await expectNoViolations(root, "aria-invalid");
  });

  it("CommandPalette — dialog + combobox + listbox roles pass axe", async () => {
    const root = mount(
      <CommandPalette
        open
        onClose={() => {}}
        actions={[
          { id: "a", title: "New file", group: "File", run: () => {} },
          { id: "b", title: "Open run", group: "Navigate", run: () => {} },
        ]}
      />,
    );
    await expectNoViolations(root, "CommandPalette");
  });
});
