import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";

import {
  type OrgSSOProvider,
  type Role,
  createOrgSSOProvider,
  deleteOrgSSOProvider,
  testOrgSSOProvider,
  updateOrgSSOProvider,
} from "@/api/orgSso";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { CopyButton } from "@/components/ui/CopyButton";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";

// OIDC default-role offers up to admin (owner is never grantable via SSO).
const OIDC_ROLES: Role[] = ["viewer", "member", "admin"];

const EMPTY_OIDC_DRAFT = {
  display_name: "",
  issuer_url: "",
  client_id: "",
  client_secret: "",
  scopes: "",
  default_role: "member" as Role,
  enabled: true,
  auto_link_on_email: false,
};

type OIDCDraft = typeof EMPTY_OIDC_DRAFT;

// OIDC scopes are stored as a string[]; the form edits them as a single
// space/comma-separated field. Empty → undefined so the server applies its
// defaults rather than persisting an empty list.
function parseScopes(s: string): string[] | undefined {
  const list = s.split(/[\s,]+/).filter(Boolean);
  return list.length > 0 ? list : undefined;
}

// draftToPayload maps the form draft to the API input — one place so add and
// edit can't drift (a field added to the draft is sent by both). An empty
// client_secret is dropped so an update keeps the stored secret.
function draftToPayload(d: OIDCDraft) {
  return {
    kind: "oidc" as const,
    display_name: d.display_name,
    issuer_url: d.issuer_url,
    client_id: d.client_id,
    client_secret: d.client_secret || undefined,
    scopes: parseScopes(d.scopes),
    default_role: d.default_role,
    auto_link_on_email: d.auto_link_on_email,
    enabled: d.enabled,
  };
}

// Keycloak/OIDC providers section. Lists existing providers (with a
// Test button that pings the IdP), and an add form below. Toggling
// enabled and deleting go through useAsyncAction so the parent's
// global err banner shows mutation failures with a single channel.
export function KeycloakSection({
  teamID,
  canManage,
  rows,
  onChange,
  onError,
}: {
  teamID: string;
  canManage: boolean;
  rows: OrgSSOProvider[];
  onChange: () => void;
  onError: (m: string) => void;
}) {
  const { confirm, dialog } = useConfirm();
  const [draft, setDraft] = useState(EMPTY_OIDC_DRAFT);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState<OIDCDraft>(EMPTY_OIDC_DRAFT);
  const [testResult, setTestResult] = useState<
    Record<string, { ok: boolean; text: string }>
  >({});
  const { busy, run } = useAsyncAction();

  const add = (ev: React.FormEvent) => {
    ev.preventDefault();
    void run(async () => {
      try {
        await createOrgSSOProvider(teamID, draftToPayload(draft));
        setDraft(EMPTY_OIDC_DRAFT);
        onChange();
      } catch (e) {
        onError(errorMessage(e));
      }
    });
  };

  const startEdit = (row: OrgSSOProvider) => {
    setEditingId(row.id);
    setEditDraft({
      display_name: row.display_name ?? "",
      issuer_url: row.issuer_url ?? "",
      client_id: row.client_id ?? "",
      client_secret: "", // blank keeps the stored secret
      scopes: (row.scopes ?? []).join(" "),
      default_role: row.default_role ?? "member",
      enabled: row.enabled,
      auto_link_on_email: row.auto_link_on_email ?? false,
    });
  };

  const saveEdit = (row: OrgSSOProvider) =>
    run(async () => {
      try {
        // client_secret is dropped when blank → the stored secret is kept.
        await updateOrgSSOProvider(teamID, row.id, draftToPayload(editDraft));
        setEditingId(null);
        onChange();
      } catch (e) {
        onError(errorMessage(e));
      }
    });

  const toggle = (row: OrgSSOProvider) =>
    run(async () => {
      try {
        await updateOrgSSOProvider(teamID, row.id, {
          kind: "oidc",
          display_name: row.display_name,
          issuer_url: row.issuer_url,
          client_id: row.client_id,
          scopes: row.scopes,
          default_role: row.default_role,
          auto_link_on_email: row.auto_link_on_email,
          enabled: !row.enabled,
        });
        onChange();
      } catch (e) {
        onError(errorMessage(e));
      }
    });

  const test = async (row: OrgSSOProvider) => {
    setTestResult((r) => ({ ...r, [row.id]: { ok: false, text: "Testing…" } }));
    try {
      const res = await testOrgSSOProvider(teamID, row.id);
      setTestResult((r) => ({
        ...r,
        [row.id]: res.ok
          ? { ok: true, text: "✓ Issuer reachable" }
          : { ok: false, text: `✗ ${res.error ?? "failed"}` },
      }));
    } catch (e) {
      setTestResult((r) => ({ ...r, [row.id]: { ok: false, text: `✗ ${errorMessage(e)}` } }));
    }
  };

  const remove = async (row: OrgSSOProvider) => {
    const ok = await confirm({
      title: "Delete SSO provider?",
      message: `Remove "${row.display_name || row.issuer_url}"? Members who sign in via it will lose that path.`,
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(async () => {
      try {
        await deleteOrgSSOProvider(teamID, row.id);
        onChange();
      } catch (e) {
        onError(errorMessage(e));
      }
    });
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      {dialog}
      <div>
        <h3 className="font-medium">Keycloak / OIDC</h3>
        <p className="text-sm text-fg-muted">
          Let members sign in via your own OpenID Connect provider. On the login page they just
          type their work email — if its domain is verified below, this provider appears
          automatically (no slug to remember).
        </p>
      </div>

      {rows.length === 0 ? (
        <div className="text-sm text-fg-muted">No OIDC providers configured.</div>
      ) : (
        <ul className="space-y-2">
          {rows.map((row) => {
            const tr = testResult[row.id];
            return (
            <li
              key={row.id}
              className="border border-border-subtle rounded p-3 text-sm space-y-2 bg-surface-0"
            >
              {editingId === row.id ? (
                <OIDCFields
                  idPrefix={`edit-${row.id}`}
                  draft={editDraft}
                  setDraft={setEditDraft}
                  secretPlaceholder="Leave blank to keep the current secret"
                  secretRequired={false}
                />
              ) : (
                <>
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="font-medium">{row.display_name || "SSO"}</span>
                    <Badge variant={row.enabled ? "success" : "neutral"}>
                      {row.enabled ? "enabled" : "disabled"}
                    </Badge>
                    {row.auto_link_on_email && <Badge variant="neutral">auto-link</Badge>}
                    <span className="text-fg-muted font-mono text-xs break-all">
                      {row.issuer_url}
                    </span>
                  </div>
                  {row.scopes && row.scopes.length > 0 && (
                    <div className="text-xs text-fg-muted">
                      Scopes: <span className="font-mono">{row.scopes.join(" ")}</span>
                    </div>
                  )}
                  {row.redirect_uri && (
                    <div className="flex items-center gap-2 text-xs text-fg-muted flex-wrap">
                      <span>Redirect URI — paste this into your IdP's allowed redirect URIs:</span>
                      <span className="font-mono break-all">{row.redirect_uri}</span>
                      <CopyButton value={row.redirect_uri} label="Copy" copiedLabel="Copied" />
                    </div>
                  )}
                  {tr && (
                    <div
                      className={`text-xs ${tr.ok ? "text-success-fg" : "text-danger-fg"}`}
                    >
                      {tr.text}
                    </div>
                  )}
                </>
              )}
              {canManage && (
                <div className="flex gap-2 flex-wrap">
                  {editingId === row.id ? (
                    <>
                      <Button
                        variant="primary"
                        size="sm"
                        loading={busy}
                        onClick={() => void saveEdit(row)}
                      >
                        Save
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setEditingId(null)}>
                        Cancel
                      </Button>
                    </>
                  ) : (
                    <>
                      <Button variant="secondary" size="sm" onClick={() => void test(row)}>
                        Test
                      </Button>
                      <Button variant="secondary" size="sm" onClick={() => startEdit(row)}>
                        Edit
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => void toggle(row)}>
                        {row.enabled ? "Disable" : "Enable"}
                      </Button>
                      <Button variant="danger" size="sm" onClick={() => void remove(row)}>
                        Delete
                      </Button>
                    </>
                  )}
                </div>
              )}
            </li>
            );
          })}
        </ul>
      )}

      {canManage && (
        <form onSubmit={add} className="space-y-3 border-t border-border-subtle pt-3">
          <h4 className="text-sm font-medium">Add an OIDC provider</h4>
          <OIDCFields
            idPrefix="add"
            draft={draft}
            setDraft={setDraft}
            secretPlaceholder=""
            secretRequired
          />
          <Button variant="primary" type="submit" loading={busy}>
            Add provider
          </Button>
        </form>
      )}
    </section>
  );
}

// OIDCFields renders the shared provider fields used by both the add form and
// the in-place edit form, so the two never drift.
function OIDCFields({
  idPrefix,
  draft,
  setDraft,
  secretPlaceholder,
  secretRequired,
}: {
  idPrefix: string;
  draft: OIDCDraft;
  setDraft: React.Dispatch<React.SetStateAction<OIDCDraft>>;
  secretPlaceholder: string;
  secretRequired: boolean;
}) {
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <div>
          <FieldLabel htmlFor={`${idPrefix}-display`}>Display name</FieldLabel>
          <Input
            size="md"
            id={`${idPrefix}-display`}
            placeholder="Acme Keycloak"
            value={draft.display_name}
            onChange={(e) => setDraft((d) => ({ ...d, display_name: e.target.value }))}
          />
        </div>
        <div>
          <FieldLabel htmlFor={`${idPrefix}-issuer`}>Issuer URL</FieldLabel>
          <Input
            size="md"
            id={`${idPrefix}-issuer`}
            type="url"
            placeholder="https://sso.acme.com/realms/main"
            value={draft.issuer_url}
            onChange={(e) => setDraft((d) => ({ ...d, issuer_url: e.target.value }))}
            required
          />
        </div>
        <div>
          <FieldLabel htmlFor={`${idPrefix}-client`}>Client ID</FieldLabel>
          <Input
            size="md"
            id={`${idPrefix}-client`}
            value={draft.client_id}
            onChange={(e) => setDraft((d) => ({ ...d, client_id: e.target.value }))}
            required
          />
        </div>
        <div>
          <FieldLabel htmlFor={`${idPrefix}-secret`}>Client secret</FieldLabel>
          <Input
            size="md"
            id={`${idPrefix}-secret`}
            type="password"
            placeholder={secretPlaceholder}
            value={draft.client_secret}
            onChange={(e) => setDraft((d) => ({ ...d, client_secret: e.target.value }))}
            autoComplete="new-password"
            required={secretRequired}
          />
        </div>
        <div>
          <FieldLabel htmlFor={`${idPrefix}-scopes`}>Scopes</FieldLabel>
          <Input
            size="md"
            id={`${idPrefix}-scopes`}
            placeholder="openid profile email (default)"
            value={draft.scopes}
            onChange={(e) => setDraft((d) => ({ ...d, scopes: e.target.value }))}
          />
        </div>
        <div>
          <FieldLabel htmlFor={`${idPrefix}-role`}>Default role</FieldLabel>
          <Select
            size="md"
            id={`${idPrefix}-role`}
            value={draft.default_role}
            onChange={(e) => setDraft((d) => ({ ...d, default_role: e.target.value as Role }))}
          >
            {OIDC_ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </Select>
        </div>
      </div>
      <Checkbox
        label="Enabled"
        checked={draft.enabled}
        onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
      />
      <Checkbox
        label="Auto-link existing accounts whose email is at a verified domain"
        help="When on, a user who already has an iterion account is linked automatically on first SSO login — but only for email domains you've verified below. Otherwise they connect it themselves from the avatar menu → Account settings → SSO connections."
        checked={draft.auto_link_on_email}
        onChange={(e) => setDraft((d) => ({ ...d, auto_link_on_email: e.target.checked }))}
      />
    </div>
  );
}
