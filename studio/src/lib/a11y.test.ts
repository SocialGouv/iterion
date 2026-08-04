// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";

import { focusableWithin, trapTabKey } from "./a11y";

afterEach(() => {
  document.body.innerHTML = "";
});

function mount(html: string): HTMLElement {
  const root = document.createElement("div");
  root.tabIndex = -1;
  root.innerHTML = html;
  document.body.appendChild(root);
  return root;
}

// trapTabKey takes a React KeyboardEvent; only `key`, `shiftKey` and
// `preventDefault` are read, so a minimal stand-in keeps the test honest
// without a full React render.
function tabEvent(shiftKey = false) {
  const preventDefault = vi.fn();
  return {
    event: { key: "Tab", shiftKey, preventDefault } as unknown as Parameters<
      typeof trapTabKey
    >[0],
    preventDefault,
  };
}

describe("focusableWithin", () => {
  it("collects the tabbable descendants in DOM order", () => {
    const root = mount(`
      <a href="#one">one</a>
      <button>two</button>
      <input />
      <textarea></textarea>
      <select></select>
      <div tabindex="0" id="grip"></div>
    `);
    expect(focusableWithin(root).map((el) => el.tagName.toLowerCase())).toEqual([
      "a",
      "button",
      "input",
      "textarea",
      "select",
      "div",
    ]);
  });

  it("skips disabled, tabindex=-1, hidden and aria-hidden elements", () => {
    const root = mount(`
      <button disabled>no</button>
      <button tabindex="-1">no</button>
      <button hidden>no</button>
      <button aria-hidden="true">no</button>
      <a>no href</a>
      <button id="yes">yes</button>
    `);
    expect(focusableWithin(root).map((el) => el.id)).toEqual(["yes"]);
  });
});

describe("trapTabKey", () => {
  it("ignores anything that is not Tab, and a null root", () => {
    const root = mount("<button>a</button>");
    const nonTab = {
      key: "ArrowLeft",
      shiftKey: false,
      preventDefault: vi.fn(),
    } as unknown as Parameters<typeof trapTabKey>[0];
    expect(trapTabKey(nonTab, root)).toBe(false);
    expect(trapTabKey(tabEvent().event, null)).toBe(false);
  });

  it("wraps forward from the last focusable to the first", () => {
    const root = mount('<button id="a">a</button><button id="b">b</button>');
    document.getElementById("b")?.focus();
    const { event, preventDefault } = tabEvent();
    expect(trapTabKey(event, root)).toBe(true);
    expect(preventDefault).toHaveBeenCalled();
    expect(document.activeElement?.id).toBe("a");
  });

  it("wraps backward from the first focusable to the last", () => {
    const root = mount('<button id="a">a</button><button id="b">b</button>');
    document.getElementById("a")?.focus();
    const { event } = tabEvent(true);
    expect(trapTabKey(event, root)).toBe(true);
    expect(document.activeElement?.id).toBe("b");
  });

  it("lets a mid-ring Tab through to the browser", () => {
    const root = mount(
      '<button id="a">a</button><button id="b">b</button><button id="c">c</button>',
    );
    document.getElementById("b")?.focus();
    const { event, preventDefault } = tabEvent();
    expect(trapTabKey(event, root)).toBe(false);
    expect(preventDefault).not.toHaveBeenCalled();
    expect(document.activeElement?.id).toBe("b");
  });

  it("enters at the right end when focus rests on the container itself", () => {
    const root = mount('<button id="a">a</button><button id="b">b</button>');
    root.focus();
    expect(trapTabKey(tabEvent().event, root)).toBe(true);
    expect(document.activeElement?.id).toBe("a");

    root.focus();
    expect(trapTabKey(tabEvent(true).event, root)).toBe(true);
    expect(document.activeElement?.id).toBe("b");
  });

  it("keeps Tab on the container when it holds nothing focusable", () => {
    const root = mount("<p>nothing to focus</p>");
    const { event, preventDefault } = tabEvent();
    expect(trapTabKey(event, root)).toBe(true);
    expect(preventDefault).toHaveBeenCalled();
    expect(document.activeElement).toBe(root);
  });
});
