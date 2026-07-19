import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { FeatureUnavailableError } from "@/api/client";
import {
  type OrgDomain,
  addOrgDomain,
  deleteOrgDomain,
  listOrgDomains,
  verifyOrgDomain,
} from "@/api/orgSso";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";

// Verified email domains — the gate per-org auto-link uses to decide
// whether a fresh OIDC subject can be glued to an existing iterion
// account. Domains start `pending` (DNS TXT record published but not
// yet observed) and flip to `verified` on a successful poll.
export function DomainsSection({
  teamID,
  canManage,
  onError,
}: {
  teamID: string;
  canManage: boolean;
  onError: (m: string) => void;
}) {
  const { confirm, dialog } = useConfirm();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState("");
  const [verifying, setVerifying] = useState<Record<string, boolean>>({});
  const [verifyMsg, setVerifyMsg] = useState<Record<string, string>>({});
  const { busy, run } = useAsyncAction();

  const query = useQuery({
    queryKey: ["org-domains", teamID],
    queryFn: () => listOrgDomains(teamID),
  });
  const domains = query.data ?? [];
  const unavailable = query.error instanceof FeatureUnavailableError;
  // The section has no banner of its own — load failures surface on the
  // parent SSO tab's error channel, like every mutation failure here.
  const loadError = query.error;
  useEffect(() => {
    if (loadError && !(loadError instanceof FeatureUnavailableError)) {
      onError(errorMessage(loadError));
    }
  }, [loadError, onError]);

  const reload = () =>
    queryClient.invalidateQueries({ queryKey: ["org-domains", teamID] });

  if (unavailable) {
    return null; // the SSO tab already surfaces the "not enabled" banner
  }

  const add = (ev: React.FormEvent) => {
    ev.preventDefault();
    void run(async () => {
      try {
        await addOrgDomain(teamID, draft);
        setDraft("");
        await reload();
      } catch (e) {
        onError(errorMessage(e));
      }
    });
  };

  // verify polls the backend a few times before giving up: DNS propagation is
  // often slower than a single click, so one immediate check then a handful of
  // spaced retries spares the admin from manually re-clicking Verify.
  const verify = async (d: OrgDomain) => {
    const attempts = 5;
    const delayMs = 3000;
    setVerifying((v) => ({ ...v, [d.id]: true }));
    setVerifyMsg((m) => ({ ...m, [d.id]: "Checking DNS…" }));
    try {
      for (let i = 0; i < attempts; i++) {
        const r = await verifyOrgDomain(teamID, d.id);
        if (r.verified) {
          setVerifyMsg((m) => ({ ...m, [d.id]: "" }));
          await reload();
          return;
        }
        if (i < attempts - 1) {
          setVerifyMsg((m) => ({
            ...m,
            [d.id]: `Not visible yet — retrying (${i + 1}/${attempts - 1})…`,
          }));
          await new Promise((res) => setTimeout(res, delayMs));
        }
      }
      setVerifyMsg((m) => ({
        ...m,
        [d.id]: `Still can't see the TXT record at ${d.challenge_host}. Confirm it's published at your DNS provider (as a TXT record, value exactly as shown) — propagation can take several minutes — then try again.`,
      }));
    } catch (e) {
      setVerifyMsg((m) => ({ ...m, [d.id]: "" }));
      onError(errorMessage(e));
    } finally {
      setVerifying((v) => ({ ...v, [d.id]: false }));
    }
  };

  const remove = async (d: OrgDomain) => {
    const ok = await confirm({
      title: "Remove domain?",
      message: `Remove ${d.domain}? Auto-link for addresses at this domain will stop.`,
      confirmLabel: "Remove",
      confirmVariant: "danger",
    });
    if (!ok) return;
    await run(async () => {
      try {
        await deleteOrgDomain(teamID, d.id);
        await reload();
      } catch (e) {
        onError(errorMessage(e));
      }
    });
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      {dialog}
      <div>
        <h3 className="font-medium">Verified email domains</h3>
        <p className="text-sm text-fg-muted">
          Prove you control an email domain (via a DNS TXT record) to allow SSO auto-link for its
          addresses. Required for the "auto-link" option above to take effect.
        </p>
      </div>

      {domains.length === 0 ? (
        <div className="text-sm text-fg-muted">No domains.</div>
      ) : (
        <ul className="space-y-2">
          {domains.map((d) => (
            <li
              key={d.id}
              className="border border-border-subtle rounded p-3 text-sm space-y-2 bg-surface-0"
            >
              <div className="flex items-center gap-2 flex-wrap">
                <span className="font-medium">{d.domain}</span>
                <Badge variant={d.verified_at ? "success" : "neutral"}>
                  {d.verified_at ? "verified" : "pending"}
                </Badge>
              </div>
              {!d.verified_at && (
                <div className="text-xs text-fg-muted space-y-1">
                  <div>Publish this DNS TXT record, then click Verify:</div>
                  <div className="flex items-center gap-2">
                    <span className="font-mono break-all">{d.challenge_host}</span>
                    <CopyButton value={d.challenge_host} label="Copy" copiedLabel="Copied" />
                  </div>
                  <div className="flex items-center gap-2">
                    <span className="font-mono break-all">{d.challenge_value}</span>
                    <CopyButton value={d.challenge_value} label="Copy" copiedLabel="Copied" />
                  </div>
                </div>
              )}
              {verifyMsg[d.id] && (
                <div className="text-xs text-fg-muted">{verifyMsg[d.id]}</div>
              )}
              {canManage && (
                <div className="flex gap-2">
                  {!d.verified_at && (
                    <Button
                      variant="secondary"
                      size="sm"
                      loading={!!verifying[d.id]}
                      onClick={() => void verify(d)}
                    >
                      {verifying[d.id] ? "Checking…" : "Verify"}
                    </Button>
                  )}
                  <Button variant="danger" size="sm" onClick={() => void remove(d)}>
                    Remove
                  </Button>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}

      {canManage && (
        <form onSubmit={add} className="flex gap-2 items-end border-t border-border-subtle pt-3">
          <div className="flex-1">
            <FieldLabel htmlFor="sso-domain">Domain</FieldLabel>
            <Input
              size="md"
              id="sso-domain"
              placeholder="acme.com"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              required
            />
          </div>
          <Button variant="primary" type="submit" loading={busy}>
            Add domain
          </Button>
        </form>
      )}
    </section>
  );
}
