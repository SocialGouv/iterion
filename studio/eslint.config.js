// @ts-check
// Flat ESLint config for the studio SPA (ESLint 10, typescript-eslint 8,
// eslint-plugin-react-hooks 7, React 19 + Vite).
//
// Calibration is deliberately pragmatic: rules that catch *real bugs* stay
// `error` (the recommended sets + react-hooks rules-of-hooks); rules that
// merely surface *debt* (any, non-null `!`, exhaustive-deps, stray console,
// react-refresh boundary) are downgraded to `warn`. So `pnpm lint` exits 0
// while still listing the debt — it can be a CI gate without a flag-day of
// fixes, and the warnings are the backlog. Non-type-aware (fast); typed
// linting (no-floating-promises etc.) is a possible follow-up.

import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    // Replaces .eslintignore — build output and generated assets.
    ignores: ["dist", "src/server/static", "**/*.d.ts"],
  },
  {
    files: ["src/**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat["recommended-latest"],
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: "module",
      globals: { ...globals.browser, ...globals.es2022 },
    },
    rules: {
      // TypeScript already proves identifiers resolve — `no-undef` is
      // redundant for .ts/.tsx and produces false positives on DOM/lib
      // globals (typescript-eslint's standing recommendation).
      "no-undef": "off",
      // Preserving the caught error as `cause` is good practice, but the
      // studio's TS lib predates the ES2022 `Error(msg, { cause })` overload
      // (bumping it is out of scope here) — keep it advisory.
      "preserve-caught-error": "warn",
      // Debt, not bugs — visible as warnings, fixed opportunistically.
      "@typescript-eslint/no-explicit-any": "warn",
      "@typescript-eslint/no-non-null-assertion": "warn",
      "@typescript-eslint/no-unused-vars": [
        "warn",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "no-console": ["warn", { allow: ["warn", "error"] }],
      // Fast-Refresh boundary hygiene (a file exporting both a component and
      // helpers/constants). A DX nicety, not a correctness bug — advisory.
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
      // Fixing every exhaustive-deps finding can change render behaviour;
      // keep it advisory and fix the high-value ones by hand.
      "react-hooks/exhaustive-deps": "warn",
      // react-hooks 7 bundles the React Compiler ruleset. The two below
      // catch genuine bugs (hook-order, render-loop) and stay `error`; the
      // rest are compiler-readiness lint — real, but behaviour-changing to
      // satisfy, so they ride as `warn` (a backlog, not a flag-day gate).
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/set-state-in-render": "error",
      "react-hooks/static-components": "warn",
      "react-hooks/use-memo": "warn",
      "react-hooks/void-use-memo": "warn",
      "react-hooks/preserve-manual-memoization": "warn",
      "react-hooks/immutability": "warn",
      "react-hooks/globals": "warn",
      "react-hooks/refs": "warn",
      "react-hooks/set-state-in-effect": "warn",
      "react-hooks/error-boundaries": "warn",
      "react-hooks/purity": "warn",
      "react-hooks/config": "warn",
      "react-hooks/gating": "warn",
    },
  },
  {
    // Tests run under node + jsdom; allow test globals and loosen `any`.
    files: ["src/**/*.{test,spec}.{ts,tsx}", "src/test/**", "src/**/*.test.ts"],
    languageOptions: {
      globals: { ...globals.node },
    },
    rules: {
      "@typescript-eslint/no-explicit-any": "off",
    },
  },
  {
    // Anti-cookie-leak boundary for the shell-less config-share editor.
    // Views under src/views/ConfigShare/** MUST NOT import the shared API
    // client — see the doc-comment at the top of src/api/configShare.ts.
    // Their fetches go through src/api/configShare.ts, which pins
    // credentials: "omit" and never reads or writes the operator's cookie.
    files: ["src/views/ConfigShare/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          paths: [
            {
              name: "@/api/client",
              message:
                "The config-share editor is shell-less and self-authenticating; use @/api/configShare (credentials: 'omit', Bearer token) instead of the shared cookie-carrying client.",
            },
          ],
          patterns: [
            {
              group: [
                "**/api/client",
                "**/api/client.ts",
                "@/api/configShareAdmin",
                "**/configShareAdmin",
              ],
              message:
                "The config-share editor must not reach the operator-authed API surface; use @/api/configShare only.",
            },
          ],
        },
      ],
    },
  },
);
