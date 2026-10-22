import { describe, expect, it } from "vitest";
import { classedIn } from "./_jsx";

// The className reader behind the source-discipline pairing rules, on
// hand-written sources: each shape a className takes in the tree, read
// as the variants a pairing rule is judged on. A rule is only as good as
// this reader — a shape it misreads is a site the rule never sees.

const norm = (v: string) => v.trim().split(/\s+/).filter(Boolean).join(" ");
const variantsOf = (src: string): string[][] =>
  classedIn("/src/x.tsx", src).map((e) => [...new Set(e.variants.map(norm))].sort());
const only = (src: string): string[] => {
  const all = variantsOf(src);
  expect(all).toHaveLength(1);
  return all[0] ?? [];
};

describe("a11y / _jsx className reader", () => {
  it("reads a string literal as one variant", () => {
    expect(only(`export const X = () => <div className="px-2 text-fg-muted" />;`)).toEqual(["px-2 text-fg-muted"]);
  });

  it("reads both arms of a ternary as two variants", () => {
    expect(only(`export const X = ({ on }: { on: boolean }) => <div className={on ? "bg-accent text-fg-onAccent" : "text-fg-muted"} />;`)).toEqual(
      ["bg-accent text-fg-onAccent", "text-fg-muted"],
    );
  });

  it("keeps a const shared by sibling arms in both variants", () => {
    // The arms are read on independent paths: a const the first arm
    // resolved is still resolved by the second.
    const src = `const base = "px-2";
export const X = ({ on }: { on: boolean }) => (
  <div className={on ? \`\${base} bg-accent text-fg-onAccent\` : \`\${base} text-fg-muted\`} />
);`;
    expect(only(src)).toEqual(["px-2 bg-accent text-fg-onAccent", "px-2 text-fg-muted"]);
  });

  it("reads MAP[k].prop as one of the map's prop values, never the map's keys", () => {
    const src = `const TONE = {
  danger: { text: "text-danger-fg", bg: "bg-danger-soft" },
  info: { text: "text-info-fg", bg: "bg-info-soft" },
};
export const X = ({ k }: { k: keyof typeof TONE }) => <span className={\`\${TONE[k].text} font-mono\`} />;`;
    expect(only(src)).toEqual(["text-danger-fg font-mono", "text-info-fg font-mono"]);
  });

  it("reads MAP[k] over string values as one of the values", () => {
    const src = `const V = { a: "text-danger", b: "text-info" };
export const X = ({ k }: { k: "a" | "b" }) => <span className={V[k]} />;`;
    expect(only(src)).toEqual(["text-danger", "text-info"]);
  });

  it("reads CONST.key as that key's value only", () => {
    const src = `const V = { primary: "bg-accent text-fg-onAccent", ghost: "bg-transparent" };
export const X = () => <button className={V.primary} />;`;
    expect(only(src)).toEqual(["bg-accent text-fg-onAccent"]);
  });

  it("reads a clsx object as optional keys", () => {
    const src = `import clsx from "clsx";
export const X = ({ dim }: { dim: boolean }) => <li className={clsx("row", { "text-fg-subtle": dim })} />;`;
    expect(only(src)).toEqual(["row", "row text-fg-subtle"]);
  });

  it("reads an array joined into a className, element by element", () => {
    const src = `export const X = ({ err }: { err: boolean }) => (
  <div className={["box", err ? "border-danger" : "", "p-3"].filter(Boolean).join(" ")} />
);`;
    expect(only(src)).toEqual(["box border-danger p-3", "box p-3"]);
  });

  it("reads cond && 'x' as present or absent", () => {
    const src = `export const X = ({ on }: { on: boolean }) => <div className={\`a \${on && "b"}\`} />;`;
    expect(only(src)).toEqual(["a", "a b"]);
  });

  it("folds arms that yield the same text into one variant", () => {
    const src = `export const X = ({ a, b }: { a: boolean; b: boolean }) => (
  <div className={\`row \${a ? "x" : "x"} \${b ? "y" : ""}\`} />
);`;
    const [e] = classedIn("/src/x.tsx", src);
    expect(e?.variants).toHaveLength(2);
    expect(only(src)).toEqual(["row x", "row x y"]);
  });

  it("collapses a variant product past the cap to the union and says so", () => {
    // Ten independent binary arms = 1024 distinct variants, past the cap.
    const n = 10;
    const arms = Array.from({ length: n }, (_, i) => `\${p${i} ? "a${i}" : "b${i}"}`).join(" ");
    const props = Array.from({ length: n }, (_, i) => `p${i}: boolean`).join("; ");
    const src = `export const X = ({ ${Array.from({ length: n }, (_, i) => `p${i}`).join(", ")} }: { ${props} }) => <div className={\`${arms}\`} />;`;
    const [e] = classedIn("/src/x.tsx", src);
    expect(e?.collapsed).toBe(true);
    expect(e?.variants).toHaveLength(1);
    for (let i = 0; i < n; i++) expect(e?.classes).toMatch(new RegExp(`\\ba${i}\\b`));
  });

  it("does not collapse nine independent arms (512 distinct variants fit the cap)", () => {
    const n = 9;
    const arms = Array.from({ length: n }, (_, i) => `\${p${i} ? "a${i}" : "b${i}"}`).join(" ");
    const props = Array.from({ length: n }, (_, i) => `p${i}: boolean`).join("; ");
    const src = `export const X = ({ ${Array.from({ length: n }, (_, i) => `p${i}`).join(", ")} }: { ${props} }) => <div className={\`${arms}\`} />;`;
    const [e] = classedIn("/src/x.tsx", src);
    expect(e?.collapsed).toBe(false);
    expect(e?.variants).toHaveLength(512);
  });

  it("reads a numeric inline opacity and the static type of an input", () => {
    const src = `export const X = () => (
  <>
    <span style={{ opacity: 0.7 }}>x</span>
    <input type="checkbox" className="h-4" />
    <input type="text" className="h-8" />
  </>
);`;
    const all = classedIn("/src/x.tsx", src);
    expect(all.map((e) => [e.tag, e.styleOpacity, e.inputType])).toEqual([
      ["span", 0.7, undefined],
      ["input", undefined, "checkbox"],
      ["input", undefined, "text"],
    ]);
  });
});
