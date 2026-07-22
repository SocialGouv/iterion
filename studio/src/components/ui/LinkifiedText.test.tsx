// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { LinkifiedText } from "./LinkifiedText";

afterEach(cleanup);

describe("LinkifiedText", () => {
  it("links a bare URL", () => {
    render(<LinkifiedText text="https://github.com/SocialGouv/iterion/pull/257" />);
    const a = screen.getByRole("link");
    expect(a).toHaveProperty(
      "href",
      "https://github.com/SocialGouv/iterion/pull/257",
    );
    expect(a.getAttribute("target")).toBe("_blank");
    expect(a.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("links a URL embedded in prose and keeps the surrounding text", () => {
    const { container } = render(
      <LinkifiedText text="see http://example.com/x for details" />,
    );
    expect(screen.getByRole("link").textContent).toBe("http://example.com/x");
    expect(container.textContent).toBe("see http://example.com/x for details");
  });

  it("excludes trailing prose punctuation from the link", () => {
    const { container } = render(
      <LinkifiedText text="ticket https://example.com/a, then https://example.com/b." />,
    );
    const hrefs = screen.getAllByRole("link").map((a) => a.textContent);
    expect(hrefs).toEqual(["https://example.com/a", "https://example.com/b"]);
    expect(container.textContent).toBe(
      "ticket https://example.com/a, then https://example.com/b.",
    );
  });

  it("keeps a balanced closing paren that belongs to the URL", () => {
    render(<LinkifiedText text="https://en.wikipedia.org/wiki/Foo_(bar)" />);
    expect(screen.getByRole("link").textContent).toBe(
      "https://en.wikipedia.org/wiki/Foo_(bar)",
    );
  });

  it("renders plain text untouched when there is no URL", () => {
    const { container } = render(<LinkifiedText text="pkg/**/*.go" />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.textContent).toBe("pkg/**/*.go");
  });

  it("never links a non-http scheme", () => {
    const { container } = render(
      <LinkifiedText text="javascript:alert(1) data:text/html,x file:///etc/passwd" />,
    );
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.textContent).toBe(
      "javascript:alert(1) data:text/html,x file:///etc/passwd",
    );
  });
});
