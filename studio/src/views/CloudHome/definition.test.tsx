// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Router } from "wouter";

import CloudHome from "@/views/CloudHome";

// The sentence, read from nowhere else. A constant here and a constant in the
// component would both be "the source", and the point of the guard is that
// there is one.
const DEFINITION = "Build, run and orchestrate agentic AI workflows.";

// The three section components pull ~50 icon modules and @lobehub/icons, which
// vitest's ESM externalization cannot resolve. What is under test is the hero
// copy, so they are stubbed out.
vi.mock("./StackCompatibility", () => ({ default: () => null }));
vi.mock("./PlatformFeatures", () => ({ default: () => null }));
vi.mock("./MissionExamples", () => ({ default: () => null }));

describe("the iterion.cloud product home", () => {
  // scripts/brand/positioning-check.sh counts the sentence in the SOURCE of
  // this component, and a source count cannot tell a rendered hero from a
  // comment: moving the definition into a `//` line while replacing the hero
  // keeps the count at one, and the guard stays green while the page a visitor
  // reads no longer says what iterion is. This asserts on what is RENDERED,
  // which has no spelling to enumerate.
  it("says what iterion is, in the text a visitor actually reads", () => {
    const { container } = render(
      <Router base="">
        <CloudHome />
      </Router>,
    );
    expect(container.textContent).toContain(DEFINITION);
    cleanup();
  });

  // The home is the root for signed-in operators too, so its primary call to
  // action must not send someone who already has a session back to a login
  // form. Guards the `signedIn` wiring AuthGate passes through CloudLanding.
  it("offers the studio, not a sign-in, to an operator who already has a session", () => {
    render(
      <Router base="">
        <CloudHome signedIn />
      </Router>,
    );
    expect(screen.getAllByRole("link", { name: /Open the studio/ })[0]).toBeTruthy();
    expect(screen.queryByRole("link", { name: /^Sign in/ })).toBeNull();
    cleanup();
  });
});
