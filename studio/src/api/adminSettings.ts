// Platform runtime settings — super-admin REST client. Mirrors
// pkg/server/admin_settings_routes.go (usage-caps) and
// pkg/server/platform_settings.go (bot-roles, sandbox, bot-vars,
// platform-credentials). Every family follows the same doctrine: an env/const
// default, a DB override record, an effective resolution, all readable and
// writable without a restart. Cloud + super-admin only — the routes are
// registered only when the corresponding store is wired, so a GET on a server
// without it 404s, which guard404 turns into FeatureUnavailableError.
//
// Response shapes come from the generated OpenAPI types (schema.ts); this
// module adds no hand-rolled body shapes. PUTs apply MERGE semantics on the
// server: a field absent from the body is left untouched, an explicit null
// clears the override back to the env/const default. The helpers below send
// ONLY the caller-supplied keys so an unspecified field is never touched.

import { FeatureUnavailableError, guard404, request } from "./client";
import type { components } from "./schema";

export { FeatureUnavailableError };

export type UsageCapsView = components["schemas"]["usageCapsView"];
export type BotRolesSettingsView = components["schemas"]["botRolesSettingsView"];
export type SandboxSettingsView = components["schemas"]["sandboxSettingsView"];
export type BotVarsSettingsView = components["schemas"]["botVarsSettingsView"];
export type PlatformCredentialsSettingsView =
  components["schemas"]["platformCredentialsSettingsView"];

// ---- usage caps (percent of the deployment's own subscription window) ----

export function getUsageCaps(): Promise<UsageCapsView> {
  return guard404("admin-settings-usage-caps", () =>
    request<UsageCapsView>("/admin/settings/usage-caps"),
  );
}

// A field omitted keeps its stored override; `null` clears it back to env.
// Callers pass only what they change — the request body is built from the
// given keys, so an unspecified window is never overwritten.
export interface UsageCapsPatch {
  five_hour_pct?: number | null;
  week_pct?: number | null;
}

export function putUsageCaps(patch: UsageCapsPatch): Promise<UsageCapsView> {
  return guard404("admin-settings-usage-caps", () =>
    request<UsageCapsView>("/admin/settings/usage-caps", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  );
}

// ---- bot roles (which bot answers each webhook role) ----

export function getBotRoles(): Promise<BotRolesSettingsView> {
  return guard404("admin-settings-bot-roles", () =>
    request<BotRolesSettingsView>("/admin/settings/bot-roles"),
  );
}

// Per-field merge: a string sets the override, `null` clears it, an omitted
// field is left untouched.
export interface BotRolesPatch {
  reviewer?: string | null;
  revi_converse?: string | null;
  brancher?: string | null;
  implementer?: string | null;
}

export function putBotRoles(patch: BotRolesPatch): Promise<BotRolesSettingsView> {
  return guard404("admin-settings-bot-roles", () =>
    request<BotRolesSettingsView>("/admin/settings/bot-roles", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  );
}

// ---- sandbox (the `sandbox: auto` default image) ----

export function getSandboxSettings(): Promise<SandboxSettingsView> {
  return guard404("admin-settings-sandbox", () =>
    request<SandboxSettingsView>("/admin/settings/sandbox"),
  );
}

// `default_image`: a ref sets the override, `null` clears it back to the
// env/built-in pin.
export interface SandboxPatch {
  default_image?: string | null;
}

export function putSandboxSettings(patch: SandboxPatch): Promise<SandboxSettingsView> {
  return guard404("admin-settings-sandbox", () =>
    request<SandboxSettingsView>("/admin/settings/sandbox", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  );
}

// ---- bot vars (ITERION_* overrides applied to every run) ----

export function getBotVars(): Promise<BotVarsSettingsView> {
  return guard404("admin-settings-bot-vars", () =>
    request<BotVarsSettingsView>("/admin/settings/bot-vars"),
  );
}

// Per-key merge on the server: a string value sets the key, `null` removes it,
// a key absent from the patch keeps its stored value. The patch is a map of
// ITERION_* var name → value|null.
export type BotVarsPatch = Record<string, string | null>;

export function putBotVars(patch: BotVarsPatch): Promise<BotVarsSettingsView> {
  return guard404("admin-settings-bot-vars", () =>
    request<BotVarsSettingsView>("/admin/settings/bot-vars", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  );
}

// ---- platform credentials audience (who may draw on the platform tier) ----

export function getPlatformCredentialsSettings(): Promise<PlatformCredentialsSettingsView> {
  return guard404("admin-settings-platform-credentials", () =>
    request<PlatformCredentialsSettingsView>("/admin/settings/platform-credentials"),
  );
}

// Merge semantics: an omitted field is left untouched, so callers pass only
// what they change. `enforce` toggles the audience; `teams`/`orgs` replace the
// respective allow-list wholesale when present.
export interface PlatformCredentialsPatch {
  enforce?: boolean;
  teams?: string[];
  orgs?: string[];
}

export function putPlatformCredentialsSettings(
  patch: PlatformCredentialsPatch,
): Promise<PlatformCredentialsSettingsView> {
  return guard404("admin-settings-platform-credentials", () =>
    request<PlatformCredentialsSettingsView>("/admin/settings/platform-credentials", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  );
}
