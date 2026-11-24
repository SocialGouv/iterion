import { describe, expect, it } from "vitest";

import {
  repeatedRowActionsIn,
  unnamedInteractivesIn,
  unnamedPrimitiveSitesIn,
} from "./_interactive";

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

  it("reads the literal spelling of a htmlFor/id pair on a paired control", () => {
    // The same reader serves both scans: a <select> named by
    // `<label htmlFor="scope">` is named whichever spelling the pair uses.
    expect(
      hits(`export const X = () => (
  <div>
    <label htmlFor="scope">Scope</label>
    <select id="scope"><option>a</option></select>
  </div>
);`),
    ).toEqual([]);
    expect(
      hits(`export const X = () => (
  <div>
    <label htmlFor="scope">Scope</label>
    <select id="other"><option>a</option></select>
  </div>
);`),
    ).toEqual(["select:"]);
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

  it("gives an empty name the same verdict in EVERY spelling", () => {
    // `aria-label=""` and `aria-label={""}` render the identical DOM, so a
    // reader that took only the bare one would wave the braces through — the
    // same two-spelling rule `writtenAs` already holds for aria-hidden.
    for (const attr of [
      `aria-label={""}`,
      "aria-label={``}",
      `aria-label={"   "}`,
      `title={""}`,
    ]) {
      expect(hits(`export const X = () => <input type="text" ${attr} />;`)).toEqual(["input:"]);
    }
    // A real name in braces still names.
    expect(hits(`export const X = () => <input type="text" aria-label={"Region"} />;`)).toEqual([]);
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

// A paired element's CHILDREN are its name only where the role takes its name
// from content. The scan credited every paired INTERACTIVE tag with having
// children, which named the whole select/checkbox/radio class out of it: the
// only reason ui/Select.tsx passed while its five sibling form primitives sat
// in naming.test.ts's waiver list.
describe("a11y / interactive naming scan — children that are not a name", () => {
  it("flags a paired <select> whose only children are its options", () => {
    expect(
      hits(`export const X = ({ opts }: { opts: string[] }) => (
  <select value="a" onChange={() => {}}>
    {opts.map((o) => (
      <option key={o} value={o}>{o}</option>
    ))}
  </select>
);`),
    ).toEqual(["select:"]);
  });

  it("flags it the same way when the options are written out", () => {
    expect(
      hits(`export const X = () => (
  <select value="squash">
    <option value="squash">Squash and merge</option>
    <option value="merge">Merge commit</option>
  </select>
);`),
    ).toEqual(["select:"]);
  });

  it("flags a paired <textarea> whose child is its value", () => {
    expect(hits(`export const X = ({ v }: { v: string }) => <textarea>{v}</textarea>;`)).toEqual([
      "textarea:",
    ]);
  });

  it("still names those controls by every real mechanism", () => {
    // aria-label on the control itself…
    expect(
      hits(`export const X = ({ opts }: { opts: string[] }) => (
  <select aria-label="Scope">{opts.map((o) => <option key={o}>{o}</option>)}</select>
);`),
    ).toEqual([]);
    // …an enclosing label that names…
    expect(
      hits(`export const X = ({ opts }: { opts: string[] }) => (
  <label>
    Scope
    <select>{opts.map((o) => <option key={o}>{o}</option>)}</select>
  </label>
);`),
    ).toEqual([]);
    // …and the file-scoped htmlFor/id pair.
    expect(
      hits(`import { useId } from "react";
export const X = ({ opts }: { opts: string[] }) => {
  const id = useId();
  return (
    <div>
      <label htmlFor={id}>Scope</label>
      <select id={id}>{opts.map((o) => <option key={o}>{o}</option>)}</select>
    </div>
  );
};`),
    ).toEqual([]);
  });

  it("keeps crediting the contents of a role named BY its contents", () => {
    // A button's children are its name, runtime or literal, and a link's
    // are too — the half of the credit that was always right.
    expect(hits(`export const X = ({ label }: { label: string }) => <button>{label}</button>;`)).toEqual([]);
    expect(hits(`export const X = () => <button><span>Save</span></button>;`)).toEqual([]);
    expect(hits(`export const X = ({ t }: { t: string }) => <a href="/x">{t}</a>;`)).toEqual([]);
  });
});

// The CONSUMER half: the six name-delegating primitives are components, so
// the intrinsic scan above cannot see one of their 469 call sites. This
// reader is the only thing that judges them, which makes its own blind spots
// the guard's blind spots — and a mechanism it fails to read costs a ledger
// row that then hides whatever else lives in that surface.
const prim = (src: string) =>
  unnamedPrimitiveSitesIn("/src/x.tsx", src).map((h) => `${h.tag}:${h.why}`);

describe("a11y / primitive call-site naming scan", () => {
  it("flags a bare <Select>, <Input> or <Textarea>", () => {
    expect(prim(`export const X = ({ v, f }: { v: string; f: () => void }) => <Select value={v} onChange={f} />;`)).toEqual(
      ["Select:unnamed"],
    );
    expect(prim(`export const X = () => <Input value="" onChange={() => {}} />;`)).toEqual([
      "Input:unnamed",
    ]);
    expect(prim(`export const X = () => <Textarea value="" onChange={() => {}} />;`)).toEqual([
      "Textarea:unnamed",
    ]);
  });

  it("flags a placeholder-only field — axe's `label` rule passes it outright", () => {
    expect(prim(`export const X = () => <Input placeholder="Search secrets…" />;`)).toEqual([
      "Input:unnamed",
    ]);
  });

  it("clears one wrapped in a <label> that names, and only the first of them", () => {
    expect(
      prim(`export const X = () => (
  <label>
    <div>Scope</div>
    <Select value="a" onChange={() => {}} />
  </label>
);`),
    ).toEqual([]);
    // An HTML label names its FIRST labelable descendant and no other.
    expect(
      prim(`export const X = () => (
  <label>
    Range
    <Input value="" onChange={() => {}} />
    <Input value="" onChange={() => {}} />
  </label>
);`),
    ).toEqual(["Input:unnamed"]);
  });

  it("lets an intrinsic control spend the label's offer before a primitive", () => {
    // The two scans must agree about who consumed the single offer, or each
    // credits a control the other calls bare.
    expect(
      prim(`export const X = () => (
  <label>
    Range
    <input type="text" />
    <Select value="a" onChange={() => {}} />
  </label>
);`),
    ).toEqual(["Select:unnamed"]);
  });

  it("clears a control under a wrapper that renders a naming <label> AROUND its children", () => {
    // SettingsDrawer's `Field`: the DOM names the control exactly as an
    // enclosing <label> does, and a reader that saw only the literal tag
    // would put its 17 correctly-labelled call sites on the ledger as bare.
    const wrapper = `function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span>{label}</span>
      {children}
      {hint && <span>{hint}</span>}
    </label>
  );
}
`;
    expect(
      prim(`${wrapper}export const X = () => (
  <Field label="Name" hint="Shown in logs"><Input value="" onChange={f} placeholder="my-dispatcher" /></Field>
);`),
    ).toEqual([]);
    // The arrow spelling, a renamed prop and `props.children` are the same wrapper.
    expect(
      prim(`const Row = ({ label: text, children }: { label: string; children: React.ReactNode }) => <label>{text}{children}</label>;
export const X = () => <Row label="Name"><Select value="a" onChange={f} /></Row>;`),
    ).toEqual([]);
    expect(
      prim(`function Row(props: { label: string; children: React.ReactNode }) {
  return <label>{props.label}{props.children}</label>;
}
export const X = () => <Row label="Name"><Textarea value="" onChange={f} /></Row>;`),
    ).toEqual([]);
    // One offer, like an HTML label's: the second control under it is bare.
    expect(
      prim(`${wrapper}export const X = () => (
  <Field label="Range"><Input value="" onChange={f} /><Input value="" onChange={f} /></Field>
);`),
    ).toEqual(["Input:unnamed"]);
    // A label BESIDE the children names nothing of theirs: the house sibling
    // pattern (issueModal's Field) names by htmlFor or not at all.
    expect(
      prim(`function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label>{label}</label>
      {children}
    </div>
  );
}
export const X = () => <Field label="Name"><Input value="" onChange={f} /></Field>;`),
    ).toEqual(["Input:unnamed"]);
    // A wrapper that renders no <label> is not one either.
    expect(
      prim(`function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <div><span>{label}</span>{children}</div>;
}
export const X = () => <Field label="Name"><Input value="" onChange={f} /></Field>;`),
    ).toEqual(["Input:unnamed"]);
  });

  it("clears a control under a wrapper IMPORTED from another source of the tree", () => {
    // orgFields.tsx's `Field` wraps its children in a <label>; OrgDrawer and
    // its siblings import it, and their call sites are named by it.
    const sites = (path: string, src: string) =>
      unnamedPrimitiveSitesIn(path, src).map((h) => `${h.tag}:${h.why}`);
    const call = `export const X = () => <Field label="Name"><Input value="" onChange={f} /></Field>;`;
    expect(
      sites("/src/views/admin/orgs/x.tsx", `import { Field } from "./orgFields";\n${call}`),
    ).toEqual([]);
    // The `@/` alias and a renamed import bind the same wrapper.
    expect(
      sites(
        "/src/views/x.tsx",
        `import { Field as Wrapped } from "@/views/admin/orgs/orgFields";
export const X = () => <Wrapped label="Name"><Input value="" onChange={f} /></Wrapped>;`,
      ),
    ).toEqual([]);
    // An import this reader cannot resolve — a package, a file outside the
    // tree — names nothing: the control is bare until proven otherwise.
    expect(prim(`import { Field } from "some-package";\n${call}`)).toEqual(["Input:unnamed"]);
    expect(prim(`import { Field } from "./nowhere";\n${call}`)).toEqual(["Input:unnamed"]);
    // An export that is NOT a wrapper (orgFields' Stat) clears nothing.
    expect(
      sites(
        "/src/views/admin/orgs/x.tsx",
        `import { Stat } from "./orgFields";
export const X = () => <Stat title="t" value="v"><Input value="" onChange={f} /></Stat>;`,
      ),
    ).toEqual(["Input:unnamed"]);
  });

  it("clears the file-scoped htmlFor/id pair on a self-closing call site", () => {
    expect(
      prim(`import { useId } from "react";
export const X = () => {
  const id = useId();
  return (
    <div>
      <label htmlFor={id}>Allowed host</label>
      <Input id={id} value="" onChange={() => {}} />
    </div>
  );
};`),
    ).toEqual([]);
  });

  it("clears an id minted by an ENCLOSING render prop, and nothing else that destructures", () => {
    // FieldRow's `<label htmlFor={inputId}>` lives in FormField.tsx, so no
    // file-scoped pair can see it: the binding is the only trace left.
    expect(
      prim(`export const X = () => (
  <FieldRow label="Allowed host">
    {({ inputId }: { inputId: string }) => <Input id={inputId} value="" onChange={() => {}} />}
  </FieldRow>
);`),
    ).toEqual([]);
    // A row id destructured from DATA is not a label conduit: the arrow is a
    // call argument, not a render prop.
    expect(
      prim(`export const X = ({ rows }: { rows: { id: string }[] }) => (
  <div>{rows.map(({ id }) => <Input key={id} id={id} value="" onChange={() => {}} />)}</div>
);`),
    ).toEqual(["Input:unnamed"]);
    // And the binding clears its OWN subtree, not the file: a second control
    // whose `id` is the component's own prop is named by nobody, whatever
    // some render prop above it happens to destructure.
    expect(
      prim(`export const X = ({ id }: { id: string }) => (
  <div>
    <FieldRow label="Allowed host">
      {({ id }: { id: string }) => <Input id={id} value="" onChange={() => {}} />}
    </FieldRow>
    <Input id={id} value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual(["Input:unnamed"]);
  });

  it("clears Checkbox's and Radio's own label prop", () => {
    expect(prim(`export const X = () => <Checkbox label="Dry run" checked onChange={() => {}} />;`)).toEqual([]);
    expect(prim(`export const X = ({ l }: { l: string }) => <Radio label={l} checked onChange={() => {}} />;`)).toEqual([]);
    expect(prim(`export const X = () => <Checkbox checked onChange={() => {}} />;`)).toEqual([
      "Checkbox:unnamed",
    ]);
  });

  it("still flags an empty aria-label — it names nothing AND kills every fallback", () => {
    expect(prim(`export const X = () => <Input aria-label="" value="" onChange={() => {}} />;`)).toEqual([
      "Input:unnamed",
    ]);
    expect(prim(`export const X = () => <Checkbox label="" checked onChange={() => {}} />;`)).toEqual([
      "Checkbox:unnamed",
    ]);
    // Braced, the same empty name: the DOM is identical, so the verdict is.
    expect(prim(`export const X = () => <Input aria-label={""} value="" onChange={() => {}} />;`)).toEqual([
      "Input:unnamed",
    ]);
    expect(prim(`export const X = () => <Checkbox label={""} checked onChange={() => {}} />;`)).toEqual([
      "Checkbox:unnamed",
    ]);
    expect(prim(`export const X = () => <Checkbox label={"Dry run"} checked onChange={() => {}} />;`)).toEqual(
      [],
    );
  });

  it("REFUSES a call site whose name could arrive through a spread", () => {
    // Reading the spread as a name is the hole; reading it as unnamed lies
    // about a correct call site. It is reported as what it is.
    expect(prim(`export const X = ({ rest }: { rest: object }) => <Input {...rest} />;`)).toEqual([
      "Input:unreadable",
    ]);
    // A spread next to a real name is named, not refused.
    expect(
      prim(`export const X = ({ rest }: { rest: object }) => <Input aria-label="Host" {...rest} />;`),
    ).toEqual([]);
  });

  it("clears a paired <Select> named by aria-labelledby, and flags its options", () => {
    expect(
      prim(`export const X = ({ o }: { o: string[] }) => (
  <Select aria-labelledby="scope-h">{o.map((x) => <option key={x}>{x}</option>)}</Select>
);`),
    ).toEqual([]);
    expect(
      prim(`export const X = ({ o }: { o: string[] }) => (
  <Select>{o.map((x) => <option key={x}>{x}</option>)}</Select>
);`),
    ).toEqual(["Select:unnamed"]);
  });

  it("reads a <label htmlFor> / id pair in BOTH spellings, and keeps the two apart", () => {
    // `htmlFor="email"` beside `id="email"` is the plain-HTML idiom and the
    // majority spelling in this tree; it names its control exactly as the
    // FieldRow useId pair does, and reading only the braced one reported 79
    // correctly-labelled call sites.
    expect(
      prim(`export const X = () => (
  <div>
    <label htmlFor="email">Email</label>
    <Input id="email" value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual([]);
    expect(
      prim(`export const X = ({ id }: { id: string }) => (
  <div>
    <label htmlFor={id}>Email</label>
    <Input id={id} value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual([]);
    // Written across the two spellings, the pair is still one value.
    expect(
      prim(`export const X = () => (
  <div>
    <label htmlFor="email">Email</label>
    <Input id={"email"} value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual([]);
    // A literal is NOT the identifier that happens to spell it:
    // `htmlFor={email}` points at whatever `email` holds, so clearing
    // `id="email"` with it would credit a control nothing names.
    expect(
      prim(`export const X = ({ email }: { email: string }) => (
  <div>
    <label htmlFor={email}>Email</label>
    <Input id="email" value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual(["Input:unnamed"]);
    // And a pair that does not match names nothing either.
    expect(
      prim(`export const X = () => (
  <div>
    <label htmlFor="email">Email</label>
    <Input id="other" value="" onChange={() => {}} />
  </div>
);`),
    ).toEqual(["Input:unnamed"]);
  });

  it("leaves the intrinsic scan's verdicts to the intrinsic scan", () => {
    // A bare <button> is the other scan's business: one walk, two judged
    // populations, and neither reports the other's tags.
    expect(prim(`export const X = () => <button className="hidden" />;`)).toEqual([]);
    expect(
      unnamedPrimitiveSitesIn("/src/x.tsx", `export const X = () => <Input placeholder="x" />;`).length,
    ).toBe(1);
    expect(
      unnamedInteractivesIn("/src/x.tsx", `export const X = () => <Input placeholder="x" />;`),
    ).toEqual([]);
  });
});

// #2591 leg 1's reader: a control inside a row map whose accessible name is
// written STATICALLY announces the same thing on every row. Its findings are
// the ticket's remainder ledger, so a shape it waves through is a surface
// nobody records, and a shape it mis-reads is a row-qualified control put on
// a list of things to fix.
const row = (src: string) =>
  repeatedRowActionsIn("/src/x.tsx", src).map(
    (s) => `${s.tag}:${s.name}${s.unreadable ? ":spread" : ""}`,
  );

describe("a11y / repeated row action scan", () => {
  it("flags a statically-named control inside a row map", () => {
    expect(
      row(`export const X = ({ rows }: { rows: { id: string }[] }) => (
  <div>{rows.map((r) => <Button key={r.id} onClick={() => f(r)}>Delete</Button>)}</div>
);`),
    ).toEqual(["Button:Delete"]);
  });

  it("clears the control once the row qualifies its name", () => {
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; name: string }[] }) => (
  <div>{rows.map((r) => <Button key={r.id} aria-label={\`Delete \${r.name}\`}>Delete</Button>)}</div>
);`),
    ).toEqual([]);
    // Named by the row's own content, too: a different name per row.
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; name: string }[] }) => (
  <div>{rows.map((r) => <Button key={r.id}>Delete {r.name}</Button>)}</div>
);`),
    ).toEqual([]);
  });

  it("judges repetition, not static names — the same control outside a map is nobody's twin", () => {
    expect(row(`export const X = () => <Button onClick={f}>Delete</Button>;`)).toEqual([]);
  });

  it("reads a row control's own naming prop, not just aria-label", () => {
    // IconButton's `label` IS its aria-label, and CopyButton's `describes`
    // is the qualifier that solves this class there.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <IconButton key={r} label="Remove" onClick={() => f(r)} />)}</div>
);`),
    ).toEqual(["IconButton:Remove"]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <IconButton key={r} label={\`Remove \${r}\`} onClick={() => f(r)} />)}</div>
);`),
    ).toEqual([]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <CopyButton key={r} value={r} />)}</div>
);`),
    ).toEqual(["CopyButton:copy"]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <CopyButton key={r} value={r} describes={r} />)}</div>
);`),
    ).toEqual([]);
  });

  it("flags a per-row toggle between two fixed names — two rows in one state collide", () => {
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; on: boolean }[] }) => (
  <div>{rows.map((r) => <Button key={r.id} aria-label={r.on ? "Disable" : "Enable"} />)}</div>
);`),
    ).toEqual(["Button:Disable / Enable"]);
  });

  it("reads a name given by title, and leaves aria-labelledby alone", () => {
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <button key={r} type="button" title="Poll now" />)}</div>
);`),
    ).toEqual(["button:Poll now"]);
    // The name lives in another element: this reader does not follow it.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <button key={r} type="button" aria-labelledby={\`h-\${r}\`} />)}</div>
);`),
    ).toEqual([]);
  });

  it("reads the contents before title — the algorithm's own order", () => {
    // A content-named control announces its text, and `title` is its name
    // only when the aria pair AND the contents give none: read the other way
    // round, three working surfaces land on the ledger and two repeated
    // actions drop off it.
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; name: string }[] }) => (
  <div>{rows.map((r) => <button key={r.id} type="button" title="Open">{r.name}</button>)}</div>
);`),
    ).toEqual([]);
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; hint: string }[] }) => (
  <div>{rows.map((r) => <Button key={r.id} title={r.hint}>Delete</Button>)}</div>
);`),
    ).toEqual(["Button:Delete"]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Button key={r} title="Install this App on GitHub">Install</Button>)}</div>
);`),
    ).toEqual(["Button:Install"]);
    // Empty contents leave the title standing as the name.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Button key={r} title="Move up"><UpIcon /></Button>)}</div>
);`),
    ).toEqual(["Button:Move up"]);
  });

  it("reads the consumer's label — enclosing, through a wrapper, or by htmlFor in this file", () => {
    // "GitHub org" on every grant row is one announcement repeated per row
    // exactly as "Remove" is. The primitive scan clears these as NAMED, so
    // this reader is the only net that sees them.
    const paired = `export const X = ({ grants }: { grants: { org: string }[] }) => (
  <div>{grants.map((g, i) => (<div key={i}><FieldLabel htmlFor={\`gh-org-\${i}\`}>GitHub org</FieldLabel><Input id={\`gh-org-\${i}\`} value={g.org} onChange={f} /></div>))}</div>
);`;
    expect(row(paired)).toEqual(["Input:GitHub org"]);
    expect(prim(paired)).toEqual([]);
    const wrapped = `export const X = ({ rows }: { rows: { id: string; on: boolean }[] }) => (
  <div>{rows.map((w) => <label key={w.id}><Checkbox checked={w.on} onChange={f} /> Enabled</label>)}</div>
);`;
    expect(row(wrapped)).toEqual(["Checkbox:Enabled"]);
    expect(prim(wrapped)).toEqual([]);
    expect(
      row(`function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <label><span>{label}</span>{children}</label>;
}
export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Field key={r} label="Name"><Input value={r} onChange={f} /></Field>)}</div>
);`),
    ).toEqual(["Input:Name"]);
    // A runtime label text is the row's own name, in either mechanism.
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; name: string }[] }) => (
  <div>{rows.map((w) => <label key={w.id}><Checkbox onChange={f} /> {w.name}</label>)}</div>
);`),
    ).toEqual([]);
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; name: string }[] }) => (
  <div>{rows.map((w) => (<div key={w.id}><label htmlFor={\`c-\${w.id}\`}>{w.name}</label><Checkbox id={\`c-\${w.id}\`} onChange={f} /></div>))}</div>
);`),
    ).toEqual([]);
    // The offer goes to the FIRST labelable descendant, as in HTML: the
    // second control announces nothing, which is the unnamed class.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <label key={r}>Range <Input value={r} onChange={f} /><Input value={r} onChange={f} /></label>)}</div>
);`),
    ).toEqual(["Input:Range"]);
  });

  it("exempts a control declared out of the tree AND out of the tab order", () => {
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <button key={r} type="button" aria-hidden="true" tabIndex={-1}>Pick</button>)}</div>
);`),
    ).toEqual([]);
  });

  it("reports a control whose name could arrive through a spread", () => {
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Button key={r} {...p(r)}>Delete</Button>)}</div>
);`),
    ).toEqual(["Button:Delete:spread"]);
  });

  it("judges the form primitives and the field wrappers, not five tags", () => {
    // `<Checkbox>` and `<Input>` are the population the primitive naming
    // guard judges; a row control of theirs repeats per row exactly as a
    // `<Button>Delete</Button>` does, and the per-row Enable/Disable toggle
    // is live in TriggerList.
    expect(
      row(`export const X = ({ rows }: { rows: { id: string; on: boolean }[] }) => (
  <div>{rows.map((r) => <Checkbox key={r.id} checked={r.on} aria-label={r.on ? "Disable trigger" : "Enable trigger"} />)}</div>
);`),
    ).toEqual(["Checkbox:Disable trigger / Enable trigger"]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Input key={r} aria-label="Variable name" value={r} onChange={f} />)}</div>
);`),
    ).toEqual(["Input:Variable name"]);
    // A FormField wrapper's `label` is REQUIRED and becomes its control's
    // <label htmlFor>: "Name" on every row of a vars editor.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <TextField key={r} label="Name" value={r} onChange={f} />)}</div>
);`),
    ).toEqual(["TextField:Name"]);
    // Qualified by the row, cleared like any other control.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Input key={r} aria-label={\`Value for \${r}\`} value={r} onChange={f} />)}</div>
);`),
    ).toEqual([]);
  });

  it("leaves a control it finds NO name for to the unnamed-control scans", () => {
    // A placeholder-only field inside a row map announces nothing, which is
    // the unnamed-control class — reporting it here would claim two rows
    // share an announcement neither of them makes.
    const src = `export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <Input key={r} placeholder="value" value={r} onChange={f} />)}</div>
);`;
    expect(row(src)).toEqual([]);
    expect(prim(src)).toEqual(["Input:unnamed"]);
  });

  it("counts two controls written on ONE line as two sites", () => {
    // Keyed by the element, not by its line: a second control written beside
    // the first used to REPLACE it, leaving the ledger's count at one while
    // the surface grew a new repeated action.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => (<span key={r}><Button>Rotate</Button><Button>Delete</Button></span>))}</div>
);`),
    ).toEqual(["Button:Rotate", "Button:Delete"]);
  });

  it("keeps a static aria-label that a lower-precedence runtime title cannot change", () => {
    // `title` sits BELOW the aria pair in the accname algorithm, so the
    // control announces "Delete" on every row whatever the title holds.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <button key={r} type="button" aria-label="Delete" title={r} />)}</div>
);`),
    ).toEqual(["button:Delete"]);
    // The other direction stays undecidable: the aria-label IS the name.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <button key={r} type="button" aria-label={r} title="Delete" />)}</div>
);`),
    ).toEqual([]);
    // CopyButton CONCATENATES `label` and `describes`, so one undecidable
    // part leaves no readable name rather than the other part standing.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <CopyButton key={r} value={r} label="Copy" describes={r} />)}</div>
);`),
    ).toEqual([]);
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{rows.map((r) => <CopyButton key={r} value={r} label="Copy" describes="plan" />)}</div>
);`),
    ).toEqual(["CopyButton:Copy plan"]);
  });

  it("counts a control under nested maps once, and keeps the receiver out of the row", () => {
    expect(
      row(`export const X = ({ groups }: { groups: { id: string; rows: string[] }[] }) => (
  <div>
    {groups.map((g) => (
      <section key={g.id}>{g.rows.map((r) => <Button key={r}>Delete</Button>)}</section>
    ))}
  </div>
);`),
    ).toEqual(["Button:Delete"]);
    // The map's RECEIVER is evaluated once, not per row.
    expect(
      row(`export const X = ({ rows }: { rows: string[] }) => (
  <div>{(<Button>Refresh</Button>) && rows.map((r) => <span key={r}>{r}</span>)}</div>
);`),
    ).toEqual([]);
  });
});
