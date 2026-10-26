import { describe, expect, it } from "vitest";

import { unnamedInteractives } from "./_interactive";

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
  it("leaves no interactive element without any naming mechanism", () => {
    const hits = unnamedInteractives().filter(
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
});
