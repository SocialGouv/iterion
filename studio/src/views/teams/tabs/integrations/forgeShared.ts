import type { ConfirmOptions } from "@/hooks/useConfirm";
import type { ForgeConnection, ForgeProvider } from "@/api/forgeConnections";

// canonicalBase mirrors forge.CanonicalBaseURL (Go) so the connect form can
// match a typed base URL against a stored OAuth app's instance key.
export const DEFAULT_BASE: Record<ForgeProvider, string> = {
  gitlab: "https://gitlab.com",
  github: "https://github.com",
  forgejo: "https://codeberg.org",
};

export function canonicalBase(provider: ForgeProvider, raw: string): string {
  const s = raw.trim();
  if (!s) return DEFAULT_BASE[provider];
  const withScheme = s.includes("://") ? s : `https://${s}`;
  return withScheme.replace(/\/+$/, "");
}

// All three forges have wired admin clients (PAT + OAuth App). GitHub App
// (installation-token) is a separate connect mode handled server-side.
export const CONNECTABLE: ForgeProvider[] = ["gitlab", "github", "forgejo"];

// DEFAULT_OAUTH_SCOPES mirrors each provider's Go DefaultScopes
// (pkg/forge/<provider>/oauth.go) so the OAuth-app registration form can show
// operators exactly what an app will request BEFORE they authorize it. Keep in
// sync with the Go source (least-privilege: GitHub is `repo` only — no
// read:org). GitHub's broad `repo` is why the least-privilege GitHub-App
// connect path is recommended over an OAuth App.
export const DEFAULT_OAUTH_SCOPES: Record<ForgeProvider, string[]> = {
  gitlab: ["api"],
  github: ["repo"],
  forgejo: ["write:repository", "read:user"],
};

export function statusTone(
  status: ForgeConnection["status"],
): "success" | "warning" | "danger" {
  if (status === "active") return "success";
  if (status === "needs_reauth") return "warning";
  return "danger";
}

// The iterion-bot mascot the server embeds (pkg/brand) and serves publicly:
// the file an operator downloads for the one upload iterion cannot do itself,
// a GitHub App's logo. Plain = the account avatar; circle = the badge form.
export const BRAND_LOGO_URL = "/brand/iterion-bot.png";
export const BRAND_LOGO_CIRCLE_URL = "/brand/iterion-bot-circle.png";

// Shape of the confirm() handler threaded from useConfirm() down to the
// connection card and OAuth-apps sections — identical to
// ReturnType<typeof useConfirm>["confirm"], named so children don't each
// re-derive it.
export type ConfirmFn = (options: ConfirmOptions) => Promise<boolean>;
