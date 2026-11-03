// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import { ApiError } from "@/api/client";
import type { ServerInfo } from "@/api/types";
import { useServerInfoStore } from "@/store/serverInfo";
import LegalPage from "./index";
import { LegalLinks } from "./LegalLinks";
import { LegalSidebarLinks, LegalSitemapGroup } from "./LegalSidebarLinks";
import { accessibilityMention } from "./legalInfo";
import { expectNoViolationsStrict, setupMatchMedia } from "@/__tests__/a11y/axeHelpers";

// jsdom lacks matchMedia; the public top bar's theme toggle reads it.
setupMatchMedia();

const getLegalDocument = vi.fn();
vi.mock("@/api/legal", () => ({
  getLegalDocument: (slug: string) => getLegalDocument(slug),
}));

// The legal surface is deployment configuration (ADR-160): a document the
// operator configured has a page and a link, one they did not has neither.

const UNCONFIGURED = {
  mode: "cloud",
  legal: { accessibility: {}, documents: [] },
} as unknown as ServerInfo;

const CONFIGURED: ServerInfo = {
  mode: "cloud",
  legal: {
    contact_email: "contact@example.org",
    accessibility: { status: "non_compliant", date: "2026-11-03" },
    documents: [
      { slug: "mentions-legales", title: "Mentions légales" },
      { slug: "accessibilite", title: "Déclaration d'accessibilité" },
    ],
  },
} as ServerInfo;

function mountAt(path: string) {
  const loc = memoryLocation({ path });
  const view = render(
    <Router hook={loc.hook}>
      <Route path="/legal/:slug">
        <LegalPage />
      </Route>
    </Router>,
  );
  return { ...view, navigate: loc.navigate };
}

function legalDoc(slug: string, title: string, markdown: string) {
  return { slug, title, markdown, accessibility: {} };
}

describe("legal links", () => {
  afterEach(() => cleanup());

  it("render nothing when the deployment configured nothing", () => {
    useServerInfoStore.setState({ info: UNCONFIGURED });
    render(<LegalLinks />);
    expect(screen.queryAllByRole("link")).toHaveLength(0);
    expect(screen.queryByRole("navigation")).toBeNull();
  });

  it("render nothing on a server that predates the surface", () => {
    useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
    render(<LegalLinks />);
    expect(screen.queryAllByRole("link")).toHaveLength(0);
  });

  it("link the configured documents only, the accessibility mention and a mailto contact", () => {
    useServerInfoStore.setState({ info: CONFIGURED });
    render(<LegalLinks />);
    const nav = screen.getByRole("navigation", { name: "Informations légales" });
    expect(nav.getAttribute("lang")).toBe("fr");
    const hrefs = screen.getAllByRole("link").map((a) => a.getAttribute("href"));
    expect(hrefs).toContain("/legal/mentions-legales");
    expect(hrefs).toContain("/legal/accessibilite");
    expect(hrefs).not.toContain("/legal/cgu");
    expect(hrefs).not.toContain("/legal/confidentialite");
    expect(hrefs).toContain("mailto:contact@example.org");
    // The mention the statement model requires on the home page, linked to
    // the statement itself.
    const mention = screen.getByRole("link", { name: "Accessibilité : non conforme" });
    expect(mention.getAttribute("href")).toBe("/legal/accessibilite");
  });

  it("print the rate beside a partial conformance", () => {
    expect(accessibilityMention({ status: "partially_compliant", rate: "72.5" })).toBe(
      "Accessibilité : partiellement conforme (72.5 %)",
    );
    expect(accessibilityMention({ status: "compliant" })).toBe("Accessibilité : totalement conforme");
    expect(accessibilityMention({ status: "" })).toBeNull();
    expect(accessibilityMention(undefined)).toBeNull();
  });

  it("sidebar: one icon link when collapsed, the list when expanded, nothing unconfigured", () => {
    useServerInfoStore.setState({ info: CONFIGURED });
    const { unmount } = render(<LegalSidebarLinks collapsed />);
    expect(screen.getByRole("link", { name: "Informations légales" }).getAttribute("href")).toBe(
      "/legal/mentions-legales",
    );
    unmount();
    render(<LegalSidebarLinks collapsed={false} />);
    expect(screen.getAllByRole("link")).toHaveLength(2);
    cleanup();
    // Unconfigured: neither branch renders a link, nor an empty landmark.
    useServerInfoStore.setState({ info: UNCONFIGURED });
    for (const collapsed of [true, false]) {
      const { container } = render(<LegalSidebarLinks collapsed={collapsed} />);
      expect(screen.queryAllByRole("link")).toHaveLength(0);
      expect(screen.queryByRole("navigation")).toBeNull();
      expect(container.innerHTML).toBe("");
      cleanup();
    }
  });
});

// The sidebar, the plan du site and the restricted shells render inside the
// studio's <Router base="/studio">: a link there must still name the root
// address /legal/<slug>, never /studio/legal/<slug> (a route the studio does
// not have — the click would land on the studio home).
describe("legal links inside the studio base", () => {
  afterEach(() => cleanup());

  function underStudioBase(children: ReactNode) {
    const loc = memoryLocation({ path: "/studio/runs" });
    return render(
      <Router hook={loc.hook}>
        <Router base="/studio">{children}</Router>
      </Router>,
    );
  }

  it("the sidebar links name the root address, collapsed and expanded", () => {
    useServerInfoStore.setState({ info: CONFIGURED });
    const { unmount } = underStudioBase(<LegalSidebarLinks collapsed />);
    expect(screen.getByRole("link", { name: "Informations légales" }).getAttribute("href")).toBe(
      "/legal/mentions-legales",
    );
    unmount();
    underStudioBase(<LegalSidebarLinks collapsed={false} />);
    expect(screen.getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual([
      "/legal/mentions-legales",
      "/legal/accessibilite",
    ]);
  });

  it("the footer links and the plan du site name the root address too", () => {
    useServerInfoStore.setState({ info: CONFIGURED });
    underStudioBase(
      <>
        <LegalLinks />
        <LegalSitemapGroup />
      </>,
    );
    const hrefs = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .filter((h) => !h?.startsWith("mailto:"));
    expect(hrefs.length).toBeGreaterThan(0);
    for (const href of hrefs) expect(href).toMatch(/^\/legal\//);
  });
});

describe("the legal page", () => {
  beforeEach(() => {
    getLegalDocument.mockReset();
    useServerInfoStore.setState({ info: CONFIGURED });
  });
  afterEach(() => cleanup());

  it("renders a configured document as French markdown under its title", async () => {
    getLegalDocument.mockResolvedValue({
      slug: "mentions-legales",
      title: "Mentions légales",
      // A document's top-level heading sits one level below the page's h1.
      markdown: "# Éditeur\n\nLa **Fabrique** numérique.\n\n| Clé | Valeur |\n|---|---|\n| Hébergeur | OVH |",
      contact_email: "contact@example.org",
      accessibility: { status: "non_compliant", date: "2026-11-03" },
    });
    mountAt("/legal/mentions-legales");
    expect(await screen.findByRole("heading", { level: 1, name: "Mentions légales" })).toBeTruthy();
    expect(getLegalDocument).toHaveBeenCalledWith("mentions-legales");
    const article = screen.getByRole("article");
    expect(article.getAttribute("lang")).toBe("fr");
    expect(screen.getByRole("main").getAttribute("lang")).toBe("fr");
    // GFM is on (the table renders), raw HTML is not (no rehype-raw).
    expect(screen.getByRole("table")).toBeTruthy();
    expect(screen.getByRole("heading", { level: 2, name: "Éditeur" })).toBeTruthy();
    expect(article.querySelector("strong")?.textContent).toBe("Fabrique");
    expect(document.title).toBe("Mentions légales — iterion");
    // The metadata beside the text.
    expect(screen.getByText(/déclaration établie le 3 novembre 2026/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "contact@example.org" }).getAttribute("href")).toBe(
      "mailto:contact@example.org",
    );
  });

  it("escapes raw HTML in the operator's markdown", async () => {
    getLegalDocument.mockResolvedValue({
      slug: "cgu",
      title: "Conditions générales d'utilisation",
      markdown: "Texte <script>window.pwned = true</script> <b>gras</b>",
      accessibility: {},
    });
    mountAt("/legal/cgu");
    const article = await screen.findByRole("article");
    expect(article.querySelector("script")).toBeNull();
    expect(article.querySelector("b")).toBeNull();
    expect(article.textContent).toContain("<b>gras</b>");
  });

  it("shows the next document, never the previous one, after a navigation", async () => {
    let releaseSecond: (v: unknown) => void = () => {};
    getLegalDocument.mockImplementation((slug: string) =>
      slug === "mentions-legales"
        ? Promise.resolve(legalDoc(slug, "Mentions légales", "Texte des mentions."))
        : new Promise((resolve) => {
            releaseSecond = resolve;
          }),
    );
    const { navigate } = mountAt("/legal/mentions-legales");
    expect(await screen.findByText("Texte des mentions.")).toBeTruthy();
    act(() => navigate("/legal/accessibilite"));
    // While the second document is in flight, the first one is gone.
    expect(screen.queryByText("Texte des mentions.")).toBeNull();
    expect(screen.getByRole("status").textContent).toContain("Chargement");
    await act(async () => releaseSecond(legalDoc("accessibilite", "Déclaration d'accessibilité", "État : non conforme.")));
    expect(await screen.findByRole("heading", { level: 1, name: "Déclaration d'accessibilité" })).toBeTruthy();
    expect(document.title).toBe("Déclaration d'accessibilité — iterion");
  });

  // Scoped to the page's own landmarks: the shared PublicTopBar above them
  // is the public catalogue's header too, and is judged there.
  it("passes axe on a rendered document and its legal footer", async () => {
    getLegalDocument.mockResolvedValue(
      legalDoc("mentions-legales", "Mentions légales", "# Éditeur\n\nLa Fabrique numérique.\n\n- un\n- deux"),
    );
    mountAt("/legal/mentions-legales");
    await screen.findByRole("article");
    await expectNoViolationsStrict(screen.getByRole("main"), "LegalPage main");
    await expectNoViolationsStrict(screen.getByRole("contentinfo"), "LegalPage footer");
  });

  it("says a document is not published on a 404 instead of an empty page", async () => {
    getLegalDocument.mockRejectedValue(new ApiError(404, "API error 404: not published"));
    mountAt("/legal/cgu");
    expect(await screen.findByRole("heading", { level: 1, name: "Document non disponible" })).toBeTruthy();
    expect(screen.queryByRole("article")).toBeNull();
    expect(screen.getByRole("link", { name: "Retour à l’accueil" }).getAttribute("href")).toBe("/");
  });

  it("answers an address that names no document as missing, without a fetch", () => {
    const loc = memoryLocation({ path: "/legal/a/b" });
    render(
      <Router hook={loc.hook}>
        <LegalPage />
      </Router>,
    );
    expect(screen.getByRole("heading", { level: 1, name: "Document non disponible" })).toBeTruthy();
    expect(getLegalDocument).not.toHaveBeenCalled();
  });

  it("hands the previous tab title back when it unmounts", async () => {
    document.title = "Sign in — iterion";
    getLegalDocument.mockResolvedValue(legalDoc("mentions-legales", "Mentions légales", "Texte."));
    const { unmount } = mountAt("/legal/mentions-legales");
    await screen.findByRole("article");
    expect(document.title).toBe("Mentions légales — iterion");
    unmount();
    expect(document.title).toBe("Sign in — iterion");
  });

  it("offers a signed-in operator the studio, not a sign-in", async () => {
    getLegalDocument.mockResolvedValue(legalDoc("cgu", "Conditions générales d'utilisation", "Texte."));
    const loc = memoryLocation({ path: "/legal/cgu" });
    render(
      <Router hook={loc.hook}>
        <Route path="/legal/:slug">
          <LegalPage signedIn />
        </Route>
      </Router>,
    );
    await screen.findByRole("article");
    expect(screen.getByRole("button", { name: "Open studio" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
  });

  it("names the failure on any other error", async () => {
    getLegalDocument.mockRejectedValue(new ApiError(503, "API error 503: server draining"));
    mountAt("/legal/cgu");
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("server draining");
  });
});
