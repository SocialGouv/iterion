import { expect, test } from "@playwright/test";

import { seed } from "../lib/state";

// studio-ui.browser-pane — the run console's Browser pane is
// level-triggered off the run's own `preview_url_available` events: a tool
// node printed `[iterion] preview_url=…`, the runtime turned that into an
// event, and the pane appears (and auto-reveals) carrying that URL. The
// seeded URL is the suite's own loopback origin, so the pane's iframe
// never leaves the test server.

test("a workflow-published preview URL reveals the Browser pane", async ({
  page,
}) => {
  const { previewRunId, previewUrl } = seed();
  await page.goto(`/runs/${previewRunId}`);

  // The pane is revealed automatically the first time a preview URL
  // becomes available, so no click is needed to make the tab appear.
  const tab = page.getByRole("tab", { name: "Browser" });
  await expect(tab).toBeVisible();
  await expect(tab).toHaveAttribute("aria-selected", "true");

  // The URL bar holds exactly what the workflow published…
  await expect(
    page.getByRole("textbox", {
      name: "Enter URL or wait for the workflow to publish one",
    }),
  ).toHaveValue(previewUrl);
  // …and the escape-hatch link points at the same place.
  await expect(page.getByRole("link", { name: "open ↗" })).toHaveAttribute(
    "href",
    previewUrl,
  );
});

test("a run that published no preview URL has no Browser pane", async ({
  page,
}) => {
  const { fixtureRunId } = seed();
  await page.goto(`/runs/${fixtureRunId}`);

  // Discriminates the pane's trigger from "the tab is always there":
  // the tool+compute fixture emits no preview_url_available event.
  await expect(page.getByRole("tab", { name: "Logs" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Browser" })).toHaveCount(0);
});

// The pane's DEFAULT scope is `external`, which iframes the URL verbatim
// instead of proxying it — so the CSP's frame-src governs it directly. The two
// tests above both use the suite's OWN origin, which `frame-src 'self'` admits,
// so neither could see a policy that blocks third-party framing. Revi caught
// exactly that on the CSP change: the pane would have rendered an empty frame
// with no in-product error.
//
// A cross-origin target is enough to decide it; the port need not answer,
// because a frame-src refusal happens BEFORE the fetch. A connection error
// leaves no securitypolicyviolation, a blocked frame leaves one.
test("an external-scope pane may iframe a cross-origin URL", async ({ page }) => {
  const { previewRunId } = seed();

  await page.addInitScript(() => {
    (window as unknown as { __cspViolations: string[] }).__cspViolations = [];
    document.addEventListener("securitypolicyviolation", (e) => {
      const ev = e as SecurityPolicyViolationEvent;
      (window as unknown as { __cspViolations: string[] }).__cspViolations.push(
        `${ev.violatedDirective} blocked ${ev.blockedURI || "(inline)"}`,
      );
    });
  });

  await page.goto(`/runs/${previewRunId}`);
  const url = page.getByRole("textbox", {
    name: "Enter URL or wait for the workflow to publish one",
  });
  await expect(url).toBeVisible();

  // A different port is a different origin, which is the whole point.
  await url.fill("http://127.0.0.1:4898/");
  await url.press("Enter");

  await expect(page.locator("iframe")).toHaveAttribute("src", "http://127.0.0.1:4898/");
  await page.waitForTimeout(500);

  const violations = await page.evaluate(
    () => (window as unknown as { __cspViolations?: string[] }).__cspViolations ?? [],
  );
  expect(
    violations.filter((v) => v.startsWith("frame-src")),
    "the CSP refused a cross-origin frame — the Browser pane's default scope is broken",
  ).toEqual([]);
});
