// @vitest-environment jsdom
//
// The skill library repeats "Edit" and "Delete" per row, and its key is the
// (scope, name) pair — the same skill name can exist globally and in the
// project, which is the hostile case the fixture holds (#2591). The upsert
// dialog is mounted too: its three visible labels pointed at no control at
// all, and axe's `label` rule passes a placeholder-only field outright.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { LibrarySkill } from "@/api/skills";
import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";

const listLocalSkills = vi.fn<() => Promise<LibrarySkill[]>>();
vi.mock("@/api/skills", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/skills")>();
  return { ...actual, listLocalSkills: () => listLocalSkills() };
});

import Skills from "./Skills";

setupMatchMedia();

const SKILLS: LibrarySkill[] = [
  { name: "changelog-writer", description: "Drafts the release note", scope: "global" },
  // Same NAME, other scope — the row key is the pair.
  { name: "changelog-writer", description: "Project override", scope: "project" },
  { name: "rgaa-audit", scope: "project" },
];

function mountSkills(): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <main>
        <Skills />
      </main>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  listLocalSkills.mockReset();
  listLocalSkills.mockResolvedValue(SKILLS);
});
afterEach(cleanup);

describe("skill library naming (RGAA 11.1)", () => {
  it("names every row's action after the skill AND the scope it acts on", async () => {
    mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    for (const name of [
      "Edit changelog-writer (global)",
      "Delete changelog-writer (global)",
      "Edit changelog-writer (project)",
      "Delete changelog-writer (project)",
      "Edit rgaa-audit (project)",
      "Delete rgaa-audit (project)",
    ]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", async () => {
    mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    expect(
      screen.getByRole("button", { name: "Edit changelog-writer (global)" }).textContent,
    ).toBe("Edit");
  });

  it("leaves no two row controls sharing an accessible name", async () => {
    const container = mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    expectUniqueAccessibleNames(container, "Skills · rows", {
      roles: [{ role: "button", matching: [/^Edit /, /^Delete /] }],
      count: 6,
    });
  });

  it("gives the upsert dialog's three fields a label that points at the control", async () => {
    mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    fireEvent.click(screen.getByRole("button", { name: "Add skill" }));
    // Found BY LABEL: the <label> had no htmlFor, so these controls were
    // named by nothing — a placeholder is not a name (#2591).
    expect(await screen.findByLabelText("Name")).toBeTruthy();
    expect(screen.getByLabelText("Scope")).toBeTruthy();
    expect(screen.getByLabelText("SKILL.md")).toBeTruthy();
  });
});

describe("skill library axe", () => {
  it("Skills — loaded table, axe smoke, incompletes included", async () => {
    const container = mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    await expectNoViolationsStrict(container, "Skills");
  });

  it("Skills — the upsert dialog's form", async () => {
    mountSkills();
    await screen.findByRole("table", { name: "Skill library" });
    fireEvent.click(screen.getByRole("button", { name: "Add skill" }));
    // Scoped to the dialog's own subtree, the way every other dialog entry
    // is: Radix aria-hides the page behind an open dialog, and whether that
    // inert page is focusable is the shell's business, not this form's.
    await screen.findByLabelText("Name");
    await expectNoViolationsStrict(screen.getByRole("dialog"), "Skills (new skill dialog)");
  });
});
