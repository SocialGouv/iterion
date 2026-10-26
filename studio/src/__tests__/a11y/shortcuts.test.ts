import { afterEach, describe, expect, it } from "vitest";

import { SINGLE_KEY_SHORTCUTS, shortcutsEnabled } from "@/lib/shortcuts";
import { useUIStore } from "@/store/ui";
import { files, scan } from "./_scanner";

afterEach(() => {
  useUIStore.getState().setSingleKeyShortcuts(true);
});

// RGAA 12.10: single-key shortcuts must be disablable. The registry
// (lib/shortcuts.ts) is the single list the help dialog renders and the
// gate every bare-key branch consults.
describe("single-key shortcuts", () => {
  it("the gate mirrors the user setting", () => {
    expect(shortcutsEnabled()).toBe(true);
    useUIStore.getState().setSingleKeyShortcuts(false);
    expect(shortcutsEnabled()).toBe(false);
    useUIStore.getState().setSingleKeyShortcuts(true);
    expect(shortcutsEnabled()).toBe(true);
  });

  it("registers every shortcut with a single key and a help label", () => {
    expect(SINGLE_KEY_SHORTCUTS.length).toBeGreaterThanOrEqual(6);
    for (const s of SINGLE_KEY_SHORTCUTS) {
      expect(s.key.length, `${s.id} registers one key`).toBe(1);
      expect(s.label.length, `${s.id} carries a help label`).toBeGreaterThan(0);
    }
  });

  it("every bare single-key branch consults the gate", () => {
    // A raw printable-key branch bypasses the user's setting. A file
    // hosting one must consult shortcutsEnabled somewhere — the known
    // hosts (Toolbar, canvas + run keyboards) all do; a new rogue
    // branch shows up here.
    const hits = scan(/e\.key === "[a-z?/]+"/)
      .filter((h) => !/ctrlKey|metaKey|altKey|Escape|Enter/.test(h))
      // BoardKeyboardHelp's ? closes the help panel itself — a dismissal
      // affordance like Escape, not a shortcut; it stays live when the
      // setting is off or a help dialog could not be dismissed.
      .filter((h) => !h.includes("BoardKeyboardHelp.tsx"))
      .filter((h) => {
        const file = h.split(":")[0]!;
        const src = files.find(([p]) => p === file)?.[1] ?? "";
        // A bare import would excuse a file whose branch stayed raw: the
        // gate must be CALLED.
        return !/shortcutsEnabled\(/.test(src);
      });
    if (hits.length) {
      throw new Error(
        `bare single-key branches outside the shortcutsEnabled gate (register in lib/shortcuts.ts and gate the branch):\n${hits.join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });
});
