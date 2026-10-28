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

  it("exempts a `hidden` control declared out of the tree AND out of the tab order", () => {
    expect(
      hits(
        `export const X = () => <input type="file" accept=".botz" className="hidden" aria-hidden="true" tabIndex={-1} />;`,
      ),
    ).toEqual([]);
  });

  it("gives the same verdict to the paired form of that shape", () => {
    expect(
      hits(`export const X = () => <button className="hidden" aria-hidden="true" tabIndex={-1}></button>;`),
    ).toEqual([]);
    expect(hits(`export const X = () => <button className="hidden"></button>;`)).toEqual([
      "button:hidden",
    ]);
  });

  it("reads the expression spelling of the claim as well as the literal one", () => {
    // `aria-hidden={true}` / `tabIndex={-1}` render the identical DOM.
    expect(
      hits(`export const X = () => <input type="file" className="hidden" aria-hidden={true} tabIndex={-1} />;`),
    ).toEqual([]);
    expect(
      hits(`export const X = () => <input type="file" className="hidden" aria-hidden={true} tabIndex={0} />;`),
    ).toEqual(["input:hidden"]);
  });

  it("exempts the same claim whatever hides it visually", () => {
    // `aria-hidden` takes the element out of the tree whatever the CSS does,
    // so the utility class is not part of the claim.
    expect(
      hits(`export const X = () => <input type="file" className="sr-only" aria-hidden="true" tabIndex={-1} />;`),
    ).toEqual([]);
    expect(
      hits(`export const X = ({ c }: { c: string }) => <input type="file" className={c} aria-hidden="true" tabIndex={-1} />;`),
    ).toEqual([]);
  });

  it("does not exempt an aria-hidden control left in the tab order", () => {
    // aria-hidden plus tabbable is a violation of its own (axe
    // aria-hidden-focus), so the claim is not true of this element.
    expect(
      hits(`export const X = () => <input type="file" className="hidden" aria-hidden="true" />;`),
    ).toEqual(["input:hidden"]);
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

  it("reports a control wrapped in a label with no text of its own", () => {
    // An empty label names nothing, so the control inside it is as unnamed
    // as one standing alone.
    expect(hits(`export const X = () => (<label><input type="text" /></label>);`)).toEqual([
      "input:",
    ]);
  });

  it("counts a runtime label as text, since the scan cannot read it", () => {
    expect(
      hits(`export const X = ({ label }: { label: string }) => (<label>{label}<input type="text" /></label>);`),
    ).toEqual([]);
  });

  it("gives the enclosing label to the FIRST labelable descendant only", () => {
    // HTML associates a label with its first labelable descendant and no
    // other, so the second control under one label has no name at all — the
    // from/to row is the live shape, and axe's own implicit-label check
    // shares the blind spot, which leaves this scan as the only net.
    const src = `export const X = () => (
  <label>
    Date range
    <input type="date" className="from" />
    <input type="date" className="to" />
  </label>
);`;
    expect(hits(src)).toEqual(["input:to"]);
  });

  it("spends the offer on a control that is named another way", () => {
    // HTML gives the label to the first labelable descendant whatever that
    // control's own name is, so a sibling after it has no label to inherit.
    const src = `export const X = () => (
  <label>
    Date range
    <input type="date" aria-label="from" />
    <input type="date" className="to" />
  </label>
);`;
    expect(hits(src)).toEqual(["input:to"]);
    // And the paired form of the same markup agrees, which is the invariant
    // the two branches owe each other.
    const paired = `export const X = () => (
  <label>
    Date range
    <select aria-label="from"><option>a</option></select>
    <select className="to"></select>
  </label>
);`;
    expect(hits(paired)).toEqual(["select:to"]);
  });

  it("gives each arm of a conditional the offer whole", () => {
    // Only one arm ever renders, and each is the label's first labelable
    // descendant in its own render.
    const src = `export const X = ({ m }: { m: boolean }) => (
  <label>
    Mode
    {m ? <input type="text" className="a" /> : <input type="text" className="b" />}
  </label>
);`;
    expect(hits(src)).toEqual([]);
    // The offer is still spent for what FOLLOWS the conditional: both arms
    // consumed it, so the trailing control inherits nothing.
    const trailing = `export const X = ({ m }: { m: boolean }) => (
  <label>
    Mode
    {m ? <input type="text" className="a" /> : <input type="text" className="b" />}
    <input type="text" className="after" />
  </label>
);`;
    expect(hits(trailing)).toEqual(["input:after"]);
  });

  it("reaches the same verdict for `&&` as for a ternary with an empty arm", () => {
    // Both spell one conditional render. In the render where the control
    // appears, the sibling after it is the label's SECOND labelable
    // descendant and has no name — a defect in one render is a defect.
    const ternary = `export const X = ({ m }: { m: boolean }) => (
  <label>
    Date range
    {m ? <input type="text" className="a" /> : null}
    <input type="text" className="b" />
  </label>
);`;
    const logical = `export const X = ({ m }: { m: boolean }) => (
  <label>
    Date range
    {m && <input type="text" className="a" />}
    <input type="text" className="b" />
  </label>
);`;
    expect(hits(ternary)).toEqual(["input:b"]);
    expect(hits(logical)).toEqual(["input:b"]);
  });

  it("walks a conditional's CONDITION, which is one render and not two", () => {
    // A link, so the subject is the WALK and not the label's offer: an <a> is
    // interactive but not labelable, so it never claims a label.
    const src = `export const X = ({ pick }: { pick: (n: unknown) => boolean }) => (
  <label>
    Mode
    {pick(<a className="inCond"></a>) ? <span>a</span> : <span>b</span>}
  </label>
);`;
    expect(hits(src)).toEqual(["a:inCond"]);
  });

  it("gives input[type=hidden] no name and no offer, in either spelling", () => {
    // A hidden input is not a labelable element, so HTML hands the label to
    // the text input beside it — and the two JSX shapes must agree.
    const selfClosing = `export const X = () => (
  <label>
    Token
    <input type="hidden" className="csrf" />
    <input type="text" className="real" />
  </label>
);`;
    const paired = `export const X = () => (
  <label>
    Token
    <input type="hidden" className="csrf"></input>
    <input type="text" className="real" />
  </label>
);`;
    expect(hits(selfClosing)).toEqual([]);
    expect(hits(paired)).toEqual([]);
  });

  it("lets a label name the control it wraps with its own aria-label", () => {
    expect(
      hits(`export const X = () => (<label aria-label="Region"><input type="text" /></label>);`),
    ).toEqual([]);
  });

  it("does not take an EMPTY name for a name", () => {
    // An empty aria-label names nothing and suppresses every fallback, so it
    // is worse than its absence — axe reports the control, and so does this.
    expect(hits(`export const X = () => <input type="text" aria-label="" />;`)).toEqual([
      "input:",
    ]);
    expect(hits(`export const X = () => (<label aria-label=" "><input type="text" /></label>);`)).toEqual([
      "input:",
    ]);
  });

  it("lets a labelable element of any tag consume the offer", () => {
    const src = `export const X = () => (
  <label>
    Search
    <button className="go">Go</button>
    <input type="search" className="q" />
  </label>
);`;
    expect(hits(src)).toEqual(["input:q"]);
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

describe("a11y / interactive naming scan — elements handed over as props", () => {
  it("reads a control passed in a JSX-valued prop of a self-closing component", () => {
    // `footer={<button />}` lives in the component's ATTRIBUTES, which is the
    // only place the walk can find it: the component itself is not
    // interactive, and it has no children to descend into.
    expect(hits(`export const X = () => <Dialog footer={<button />} />;`)).toEqual(["button:"]);
  });

  it("reads it the same way on a paired component", () => {
    expect(hits(`export const X = () => <Dialog footer={<button />}>body</Dialog>;`)).toEqual([
      "button:",
    ]);
  });
});
