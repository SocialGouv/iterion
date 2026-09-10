import { expect, test } from "@playwright/test";

// studio-ui.security-headers — the CSP is enforced against the REAL SPA, not
// asserted as a string. A policy is only worth its directives if the app it
// governs still works under it, and the only honest oracle for that is the
// browser.
//
// It also pins the self-hosting of Monaco. The editor used to be fetched at
// runtime from cdn.jsdelivr.net, which put third-party executable code in the
// surface that edits LLM keys and forge tokens; `script-src 'self'` is what
// makes a regression fail loudly instead of silently phoning out again.

const CDN_HOSTS = /cdn\.jsdelivr\.net|unpkg\.com|cdnjs\.cloudflare\.com/;

/** Collects CSP violations and off-origin requests for the life of a page. */
function watch(page: import("@playwright/test").Page) {
  const violations: string[] = [];
  const offOrigin: string[] = [];
  const errors: string[] = [];

  // A worker that fails to load reports NEITHER a CSP violation NOR an
  // off-origin request — it throws inside the worker. This spec was green on
  // a build whose editor worker was dead (no diff, no suggestions, no link
  // detection), so the page's own error channel is watched too.
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(`console: ${m.text()}`);
  });

  // securitypolicyviolation fires in the page for every blocked resource.
  // Registered via addInitScript so it is armed before the first byte runs.
  page.addInitScript(() => {
    (window as unknown as { __cspViolations: string[] }).__cspViolations = [];
    document.addEventListener("securitypolicyviolation", (e) => {
      const ev = e as SecurityPolicyViolationEvent;
      (window as unknown as { __cspViolations: string[] }).__cspViolations.push(
        `${ev.violatedDirective} blocked ${ev.blockedURI || "(inline)"}`,
      );
    });
  });

  page.on("request", (req) => {
    if (CDN_HOSTS.test(req.url())) offOrigin.push(req.url());
  });

  return {
    offOrigin,
    errors,
    async drain() {
      const inPage = await page.evaluate(
        () => (window as unknown as { __cspViolations?: string[] }).__cspViolations ?? [],
      );
      return violations.concat(inPage);
    },
  };
}

test("the studio serves its security headers", async ({ page }) => {
  const res = await page.goto("/");
  expect(res).not.toBeNull();
  const headers = res!.headers();

  expect(headers["x-content-type-options"]).toBe("nosniff");
  expect(headers["referrer-policy"]).toBe("strict-origin-when-cross-origin");
  expect(headers["x-frame-options"]).toBe("SAMEORIGIN");

  const csp = headers["content-security-policy"] ?? "";
  expect(csp, "no CSP on the document").toBeTruthy();
  expect(csp).toContain("script-src 'self'");
  expect(csp).toContain("frame-ancestors 'self'");
  expect(csp).toContain("object-src 'none'");
  // script-src is the boundary between an injected string and running code:
  // neither escape hatch may appear there. (style-src does carry
  // 'unsafe-inline' — measured, see the policy comment in Go.)
  const scriptSrc = csp
    .split(";")
    .map((d) => d.trim())
    .find((d) => d.startsWith("script-src "));
  expect(scriptSrc).toBe("script-src 'self'");
  expect(csp).not.toContain("unsafe-eval");
});

test("the app boots under the CSP with no violation and no CDN request", async ({
  page,
}) => {
  const w = watch(page);

  await page.goto("/runs");
  // The seeded run must be on screen: the SPA has to boot, fetch over the API
  // and render under the policy. Without this the test would pass on a blank
  // page, which is exactly what a too-strict CSP produces.
  await expect(page.getByRole("row").filter({ hasText: "/demo-bot/main.bot" })).toContainText(
    "finished",
  );
  await page.waitForLoadState("networkidle");

  expect(await w.drain(), "CSP violations while booting the SPA").toEqual([]);
  expect(w.offOrigin, "the SPA fetched from a third-party CDN").toEqual([]);
  expect(w.errors, "the SPA logged errors while booting").toEqual([]);
});

test("Monaco loads self-hosted, under the CSP, with its workers", async ({ page }) => {
  const w = watch(page);

  await page.goto("/editor?file=bots/demo-bot/main.bot");
  // Open the source pane — that is what mounts Monaco.
  await page.getByRole("button", { name: "Toggle source view" }).click();

  // The editor really mounted: Monaco renders its own view container, and the
  // .bot source is tokenized into it. Without this the assertions below would
  // pass on a page where the editor silently failed to load.
  const monaco = page.locator(".monaco-editor").first();
  await expect(monaco).toBeVisible({ timeout: 30_000 });
  await expect(monaco).toContainText("ui_fixture", { timeout: 30_000 });

  await page.waitForLoadState("networkidle");

  expect(await w.drain(), "CSP violations after mounting Monaco").toEqual([]);
  expect(
    w.offOrigin,
    "Monaco was fetched from a CDN — loader.config({ monaco }) is not in effect",
  ).toEqual([]);
  // The oracle for the workers. A `data:`-inlined or 404ing worker surfaces
  // here as "Failed to load worker script for label: …" and nowhere else.
  expect(w.errors, "the editor logged errors (a worker failing to load?)").toEqual([]);
});

// Creating a GitHub App has no API: the studio builds a form whose action is
// the forge's own /settings/apps/new and submits it programmatically
// (CreateGitHubAppCard, RegisterOAuthAppForm). `form-action 'self'` refuses
// that cross-origin POST — and does NOT fall back to default-src — while
// form.submit() throws nothing, so the card sticks in its busy state with only
// a console violation and forge onboarding breaks silently. Revi caught it on
// the CSP; nothing else here would have.
//
// Submitted into a hidden iframe so the test page is not navigated away, and
// at a port that need not answer: a form-action refusal happens BEFORE the
// request leaves.
test("the CSP admits a cross-origin form submission (GitHub App manifest flow)", async ({
  page,
}) => {
  const w = watch(page);
  await page.goto("/");

  await page.evaluate(() => {
    const frame = document.createElement("iframe");
    frame.name = "csp-probe";
    frame.style.display = "none";
    document.body.appendChild(frame);

    const form = document.createElement("form");
    form.method = "POST";
    form.action = "http://127.0.0.1:4898/settings/apps/new";
    form.target = "csp-probe";
    document.body.appendChild(form);
    form.submit();
  });
  await page.waitForTimeout(500);

  const violations = await w.drain();
  expect(
    violations.filter((v) => v.startsWith("form-action")),
    "the CSP refused a cross-origin form POST — GitHub App creation would hang with no error",
  ).toEqual([]);
});
