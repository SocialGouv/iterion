import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import { unnamedInteractives } from "./_interactive";
import { files } from "./_scanner";

// One scan for the whole file: parsing every .tsx costs ~250 ms, and both
// guards below judge the same immutable list.
const ALL = unnamedInteractives();

// RGAA 11.1's real net (#2591 leg 2): axe 4.13 passes placeholder-only
// inputs outright, so the source scan is the class-level guard for
// interactive elements with NO naming mechanism at all. Placeholder is
// deliberately not one. Surfaces whose naming is pinned by colocated
// role-name tests list their entries here with the reason — a waiver
// names a test, never a shrug.
const WAIVED: { path: string; tag: string; reason: string }[] = [
  // The shared form primitives are named by their CONSUMERS: FieldRow's
  // useId htmlFor/id pair and the enclosing-label slots decide the name at
  // each call site — undecidable from the primitive's own source, pinned by
  // the forms' colocated role-name tests (FormField/QuestionInput suites).
  { path: "/src/components/ui/Input.tsx", tag: "input", reason: "named by the consumer's FieldRow label (useId htmlFor/id)" },
  { path: "/src/components/ui/Radio.tsx", tag: "input", reason: "named by the consumer's OptionRow label" },
  { path: "/src/components/ui/Checkbox.tsx", tag: "input", reason: "named by the consumer's OptionRow label" },
  { path: "/src/components/ui/Textarea.tsx", tag: "textarea", reason: "named by the consumer's FieldRow label (useId htmlFor/id)" },
  { path: "/src/components/ui/MarkdownPreview.tsx", tag: "textarea", reason: "a read-only preview pane, not a labelled control" },
  { path: "/src/components/ui/TagInput.tsx", tag: "input", reason: "named by the TagInput wrapper's own label region" },
  // MarkdownText renders markdown-provided links: the link text is the
  // markdown's own content, decided at runtime — the source scan cannot
  // see it, and MarkdownText's suite covers the rendering.
  { path: "/src/components/Runs/conversation/MarkdownText.tsx", tag: "a", reason: "link text comes from the rendered markdown itself" },
];

describe("interactive elements carry a naming mechanism (RGAA 11.1)", () => {
  it("GateInboundFile's download link names the file via aria-label (#2510)", () => {
    const src = readFileSync(
      resolve(process.cwd(), "src/components/Runs/conversation/GateInboundFile.tsx"),
      "utf8",
    );
    expect(src).toContain('aria-label={`Download ${label}`}');
  });

  it("leaves no interactive element without any naming mechanism", () => {
    const hits = ALL.filter(
      (h) => !WAIVED.some((w) => w.path === h.path && w.tag === h.tag),
    );
    if (hits.length) {
      throw new Error(
        `interactive elements with no naming mechanism (visible text, aria-label, aria-labelledby, title, or an enclosing label — placeholder is NOT one):\n${hits
          .map((h) => `  ${h.path}:${h.line}  <${h.tag}> ${h.classes}`)
          .join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  // A waiver is a live claim about a site. One that matches nothing is a
  // sentence nobody can check, and the entry it hides behind is how a list
  // like this one stops describing the tree it guards.
  it("carries no waiver the tree no longer needs", () => {
    const dead = WAIVED.filter(
      (w) => !ALL.some((h) => h.path === w.path && h.tag === w.tag),
    );
    if (dead.length) {
      throw new Error(
        `waivers that match nothing — the element is named now, drop them:\n${dead
          .map((w) => `  ${w.path}  <${w.tag}>: ${w.reason}`)
          .join("\n")}`,
      );
    }
    expect(dead).toHaveLength(0);
  });
});

// VarFieldInput renders a bare control for every var type and leaves the name
// to its caller's `<label htmlFor>` — the `id` prop's contract. `id` stays
// optional in the props (a control inside an enclosing label needs none), so
// the contract is a guard here rather than a type: a `bool` var whose caller
// forgot it renders a checkbox announcing nothing, and the rendered nets only
// cover the surfaces that happen to carry an axe entry.
describe("VarFieldInput's callers own the name (RGAA 11.1)", () => {
  it("hands every call site's id to a label in the same file", () => {
    const problems: string[] = [];
    let sites = 0;
    for (const [path, src] of files) {
      const opened = src.match(/<VarFieldInput\b/g)?.length ?? 0;
      if (opened === 0) continue;
      const tags = src.match(/<VarFieldInput\b[\s\S]*?\/>/g) ?? [];
      if (tags.length !== opened) {
        // A paired `<VarFieldInput>…</VarFieldInput>` would slip past the
        // reader below, so the shape fails instead of going unread.
        problems.push(`${path}  a <VarFieldInput> is not self-closing — this guard cannot read it`);
        continue;
      }
      for (const tag of tags) {
        sites += 1;
        // The match runs to the FIRST `/>`, so any nested JSX — a child under
        // a paired form, a `prop={<Icon />}` — means the span read here is
        // not the call site's own attributes. Such a shape fails rather than
        // being read wrong in either direction.
        if (tag.slice("<VarFieldInput".length).includes("<")) {
          problems.push(`${path}  a <VarFieldInput> carries nested JSX — this guard cannot read it`);
          continue;
        }
        // Anchored: `\b` makes `data-row-id={rowId}` an id, and a call site
        // with no id of its own would then be credited with another
        // attribute's expression.
        const id = /(?<![\w-])id=\{(`[^`]*`|[^}]*)\}/.exec(tag)?.[1];
        if (!id) {
          problems.push(`${path}  <VarFieldInput> with no id — nothing can point a label at it`);
          continue;
        }
        if (!src.includes(`htmlFor={${id}}`)) {
          problems.push(`${path}  <VarFieldInput id={${id}}> — no <label htmlFor={${id}}> in this file`);
        }
      }
    }
    if (problems.length) {
      throw new Error(
        `VarFieldInput call sites whose control has no name:\n${problems.join("\n")}`,
      );
    }
    // Non-vacuous: the call sites exist and were read.
    expect(sites).toBeGreaterThanOrEqual(4);
  });
});
