import { describe, expect, it } from "vitest";

import { unnamedInteractivesIn } from "./_interactive";

// The naming scan behind the RGAA 11.1 source guard, on hand-written sources.
// The guard is only as good as the shapes this reader admits: a shape it waves
// through is a site the guard cannot see, and a mechanism it fails to read is a
// correctly-named control the guard reports — which buys a waiver that then
// hides whatever else lives in that file.

const hits = (src: string) =>
  unnamedInteractivesIn("/src/x.tsx", src).map((h) => `${h.tag}:${h.classes}`);

describe("a11y / interactive naming scan — the hidden utility", () => {
  it("flags a `hidden` control that does not say it is out of the tree", () => {
    expect(
      hits(`export const X = () => <input type="file" accept=".botz" className="hidden" />;`),
    ).toEqual(["input:hidden"]);
  });

  it("exempts a `hidden` control that declares aria-hidden=\"true\"", () => {
    expect(
      hits(
        `export const X = () => <input type="file" accept=".botz" className="hidden" aria-hidden="true" tabIndex={-1} />;`,
      ),
    ).toEqual([]);
  });

  it("does not take `aria-hidden=\"false\"` for the claim: it says the opposite", () => {
    expect(
      hits(`export const X = () => <input type="file" className="hidden" aria-hidden="false" />;`),
    ).toEqual(["input:hidden"]);
  });

  it("does not take a runtime aria-hidden for the claim either", () => {
    // `aria-hidden={collapsed}` is false half the time, and which half is not
    // decidable here — so the element is judged as exposed.
    expect(
      hits(
        `export const X = ({ collapsed }: { collapsed: boolean }) => <input type="file" className="hidden" aria-hidden={collapsed} />;`,
      ),
    ).toEqual(["input:hidden"]);
  });

  it("still exempts input[type=hidden], which carries no control at all", () => {
    expect(hits(`export const X = () => <input type="hidden" name="csrf" />;`)).toEqual([]);
  });

  it("leaves a named control alone whether it is hidden or not", () => {
    expect(
      hits(`export const X = () => <input type="file" className="sr-only" aria-label="Upload plan" />;`),
    ).toEqual([]);
  });

  it("flags an unnamed control with no class at all", () => {
    expect(hits(`export const X = () => <input type="file" />;`)).toEqual(["input:"]);
  });
});

describe("a11y / interactive naming scan — the mechanisms it must read", () => {
  // An enclosing <label> is one of the five mechanisms this scan advertises,
  // and the only one whose reach depends on the walk carrying state down.
  it("reads an enclosing label around a self-closing control", () => {
    expect(hits(`export const X = () => (<label>Full name <input type="text" /></label>);`)).toEqual([]);
  });

  it("reads an enclosing label around a paired control", () => {
    expect(hits(`export const X = () => (<label>Bio <textarea></textarea></label>);`)).toEqual([]);
  });

  it("reads an enclosing label through the wrappers a real row puts in between", () => {
    const src = `export const X = () => (
  <label className="grid">
    <span className="text-xs">Region</span>
    <div className="flex">
      <input type="text" />
    </div>
  </label>
);`;
    expect(hits(src)).toEqual([]);
  });

  it("does not spill the enclosing label onto a control OUTSIDE it", () => {
    const src = `export const X = () => (
  <div>
    <label>Region <input type="text" /></label>
    <input type="text" />
  </div>
);`;
    expect(hits(src)).toEqual(["input:"]);
  });

  // The htmlFor/id pair is read for a PAIRED element only, and that
  // asymmetry is the decision, not an oversight: the match is file-scoped.
  it("reads the htmlFor/id pair on a paired control", () => {
    const src = `import { useId } from "react";
export const X = () => {
  const id = useId();
  return (
    <div>
      <label htmlFor={id}>Region</label>
      <select id={id}><option>fr</option></select>
    </div>
  );
};`;
    expect(hits(src)).toEqual([]);
  });

  it("reports a self-closing control the pair alone would name", () => {
    // A primitive with a labelled and an unlabelled branch shares one id
    // expression between them (Checkbox, Radio). Reading the file-scoped pair
    // here would let the labelled branch name the unlabelled one, and the
    // class this guard exists for would go silent; the unlabelled branch is
    // reported, and its waiver names the consumer suite that pins it.
    const src = `import { useId } from "react";
export const X = ({ label }: { label?: string }) => {
  const id = useId();
  if (!label) return <input id={id} type="checkbox" />;
  return (
    <label htmlFor={id}>
      <input id={id} type="checkbox" />
      <span>{label}</span>
    </label>
  );
};`;
    expect(hits(src)).toEqual(["input:"]);
  });
});
