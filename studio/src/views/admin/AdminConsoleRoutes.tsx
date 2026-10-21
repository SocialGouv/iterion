// The super-admin console's inner route group, mounted by App.tsx inside
// `<Route path="/admin" nest>` + <RequireSuperAdmin>. Paths are relative to
// /admin — see adminRoutes.ts, the table this and AdminNav both render.

import { lazy } from "react";
import { Redirect, Route, Switch } from "wouter";

import { ADMIN_HOME, ADMIN_PATHS, type AdminPath } from "./adminRoutes";

const OrgsAdminPage = lazy(() => import("@/views/admin/OrgsAdminPage"));
const UsersAdminPage = lazy(() => import("@/views/admin/UsersAdminPage"));
const AuditAdminPage = lazy(() => import("@/views/admin/AuditAdminPage"));
const DLQAdminPage = lazy(() => import("@/views/admin/DLQAdminPage"));
const PlatformLlmCredsPage = lazy(() => import("@/views/admin/PlatformLlmCredsPage"));
const PlatformForgeAppsPage = lazy(() => import("@/views/admin/PlatformForgeAppsPage"));
const PlatformBotsPage = lazy(() => import("@/views/admin/PlatformBotsPage"));
const UsageCapsPage = lazy(() => import("@/views/admin/settings/UsageCapsPage"));
const CredentialSpendPage = lazy(() => import("@/views/admin/spend/CredentialSpendPage"));
const BotRolesPage = lazy(() => import("@/views/admin/settings/BotRolesPage"));
const SandboxPage = lazy(() => import("@/views/admin/settings/SandboxPage"));
const BotVarsPage = lazy(() => import("@/views/admin/settings/BotVarsPage"));
const PlatformCredentialsSettingsPage = lazy(
  () => import("@/views/admin/settings/PlatformCredentialsPage"),
);

// One page per table entry. `satisfies` makes the two halves one: a table
// entry with no page here, or a page here that the nav never offers, fails to
// compile — so every screen the nav shows is mounted, and every mounted screen
// is reachable from the nav.
const PAGES = {
  "/orgs": OrgsAdminPage,
  "/users": UsersAdminPage,
  "/llm-credentials": PlatformLlmCredsPage,
  "/forge-apps": PlatformForgeAppsPage,
  "/settings/usage-caps": UsageCapsPage,
  "/settings/bot-roles": BotRolesPage,
  "/settings/sandbox": SandboxPage,
  "/settings/bot-vars": BotVarsPage,
  "/settings/platform-credentials": PlatformCredentialsSettingsPage,
  "/bots": PlatformBotsPage,
  "/audit": AuditAdminPage,
  "/dlq": DLQAdminPage,
  "/spend": CredentialSpendPage,
} satisfies Record<AdminPath, unknown>;

export default function AdminConsoleRoutes() {
  return (
    <Switch>
      {ADMIN_PATHS.map((path) => (
        <Route key={path} path={path} component={PAGES[path]} />
      ))}
      {/* Bare /admin, and any console path the table no longer declares, land
          on the console home instead of the blank page an unmatched inner
          Switch renders. ADMIN_HOME is itself a declared path, so this cannot
          redirect to itself. */}
      <Route>
        <Redirect to={ADMIN_HOME} replace />
      </Route>
    </Switch>
  );
}
