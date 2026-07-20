import { useCallback, useMemo, useState } from "react";
import { Link } from "wouter";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import type { BotEntryWithSchema, ConfigShareSpec } from "@/api/bots";
import {
  createConfigShare,
  deleteConfigShare,
  FeatureUnavailableError,
  listConfigShares,
  rotateConfigShare,
  type CreateShareInput,
  type ShareView,
  type ShareWithToken,
} from "@/api/configShareAdmin";
import { useAuth } from "@/auth/AuthContext";
import {
  Badge,
  Button,
  Card,
  Checkbox,
  CopyButton,
  Dialog,
  FieldLabel,
  InlineBanner,
  Input,
  Spinner,
} from "@/components/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";
import { useUIStore } from "@/store/ui";

import { ShareDeliveriesDrawer } from "./ShareDeliveriesDrawer";

/**
 * ConfigSharesCard — operator UI on a bot's home page: list this bot's
 * config-shares for the active team, mint a new one (surfacing token + URL
 * ONCE), rotate, and revoke.
 *
 * Gated by serverInfo.config_shares_enabled at the call site; if the
 * feature is off on this deployment the card renders nothing.
 *
 * Cloud-only in practice: creating a share needs an active team_id.
 * Locally there's no team; the parent hides the card when activeTeam is
 * missing.
 */
export function ConfigSharesCard({ entry }: { entry: BotEntryWithSchema }) {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id;

  const [creating, setCreating] = useState(false);
  const [mintedForOnce, setMintedForOnce] = useState<ShareWithToken | null>(null);
  const [busyShareID, setBusyShareID] = useState<string | null>(null);
  const [deliveriesShare, setDeliveriesShare] = useState<ShareView | null>(null);
  const { confirm, dialog: confirmDialog } = useConfirm();

  const addToast = useUIStore((s) => s.addToast);

  const queryClient = useQueryClient();
  // The list is team-wide; this card shows the rows for its bot only.
  const sharesQuery = useQuery<ShareView[]>({
    queryKey: ["config-shares", teamID],
    queryFn: () => listConfigShares(teamID ?? ""),
    enabled: !!teamID,
  });
  const shares = useMemo(
    () =>
      sharesQuery.data
        ? sharesQuery.data.filter((s) => s.bot_id === entry.name)
        : null,
    [sharesQuery.data, entry.name],
  );
  const unavailable = sharesQuery.error instanceof FeatureUnavailableError;
  // Hidden while a reload is in flight so the loading state shows instead.
  const error =
    sharesQuery.error && !unavailable && !sharesQuery.isFetching
      ? errorMessage(sharesQuery.error)
      : null;
  // Mint / rotate / revoke reload through invalidation so every consumer
  // of the team's share list refreshes.
  const reload = useCallback(
    () => queryClient.invalidateQueries({ queryKey: ["config-shares", teamID] }),
    [queryClient, teamID],
  );

  if (!teamID) return null;
  if (unavailable) return null;
  // Config-share is offered only for bots that DECLARE a shareable surface
  // (manifest config_share: block). Everything the mint form needs — the
  // config file + which fields are editable — comes from this spec, so a
  // second bot gets the card by adding the block, with no code change here.
  const spec = entry.config_share;
  if (!spec) return null;

  const onRotate = async (share: ShareView) => {
    setBusyShareID(share.id);
    try {
      const minted = await rotateConfigShare(teamID, share.id);
      setMintedForOnce(minted);
      await reload();
    } catch (err) {
      addToast(errorMessage(err), "error");
    } finally {
      setBusyShareID(null);
    }
  };

  const onDelete = async (share: ShareView) => {
    const ok = await confirm({
      title: "Revoke this share?",
      message: (
        <>
          The bookmarked link will stop working immediately.
          <br />
          <span className="text-fg-muted">
            {share.label?.trim() || share.id} ({share.category})
          </span>
        </>
      ),
      confirmLabel: "Revoke",
      confirmVariant: "danger",
    });
    if (!ok) return;
    setBusyShareID(share.id);
    try {
      await deleteConfigShare(teamID, share.id);
      addToast("Share revoked", "info");
      await reload();
    } catch (err) {
      addToast(errorMessage(err), "error");
    } finally {
      setBusyShareID(null);
    }
  };

  return (
    <Card>
      <div className="mb-2 flex items-center justify-between gap-2">
        <h2 className="text-xs font-semibold text-fg-default">Config-share links</h2>
        <div className="flex items-center gap-3">
          {/* Cross-path: edit this bot's config-shares directly in the
              signed-in config editor (pre-filtered to this bot). */}
          {(shares ?? []).length > 0 && (
            <Link
              href={`/config-editor?bot=${encodeURIComponent(entry.name)}`}
              className="whitespace-nowrap text-caption text-accent-text hover:underline"
            >
              Open in config editor →
            </Link>
          )}
          <Button variant="secondary" size="sm" onClick={() => setCreating(true)}>
            Create share link…
          </Button>
        </div>
      </div>
      <p className="mb-2 text-caption text-fg-subtle">
        Scoped, self-service editor links for a category of the bot's config
        file. The visitor only edits the fields you allow; the token stays
        live until rotated or revoked.
      </p>
      {error && (
        <InlineBanner tone="danger" layout="inline" title="Couldn't load shares">
          {error}
        </InlineBanner>
      )}
      {shares === null && !error ? (
        <div className="flex items-center gap-2 py-2 text-sm text-fg-muted">
          <Spinner /> Loading…
        </div>
      ) : (shares ?? []).length === 0 ? (
        <p className="py-1 text-xs text-fg-subtle">
          No shares yet — create one to give a non-operator scoped edit access.
        </p>
      ) : (
        <ul className="mt-1 space-y-1.5">
          {(shares ?? []).map((s) => (
            <ShareRow
              key={s.id}
              share={s}
              busy={busyShareID === s.id}
              onRotate={() => void onRotate(s)}
              onDelete={() => void onDelete(s)}
              onDeliveries={() => setDeliveriesShare(s)}
            />
          ))}
        </ul>
      )}

      {creating && teamID && (
        <CreateShareDialog
          teamID={teamID}
          botID={entry.name}
          spec={spec}
          onCancel={() => setCreating(false)}
          onCreated={(minted) => {
            setCreating(false);
            setMintedForOnce(minted);
            void reload();
          }}
        />
      )}
      {mintedForOnce && (
        <TokenOnceDialog
          minted={mintedForOnce}
          onClose={() => setMintedForOnce(null)}
        />
      )}
      {deliveriesShare && (
        <ShareDeliveriesDrawer
          teamID={teamID}
          share={deliveriesShare}
          onClose={() => setDeliveriesShare(null)}
        />
      )}
      {confirmDialog}
    </Card>
  );
}

function ShareRow({
  share,
  busy,
  onRotate,
  onDelete,
  onDeliveries,
}: {
  share: ShareView;
  busy: boolean;
  onRotate: () => void;
  onDelete: () => void;
  onDeliveries: () => void;
}) {
  const now = Date.now();
  const expiresAt = share.expires_at ? new Date(share.expires_at).getTime() : 0;
  const expired = expiresAt > 0 && expiresAt < now;
  const revoked = !!share.revoked_at;
  const disabled = revoked || expired;
  return (
    <li className="rounded-md border border-border-default bg-surface-2 px-2 py-1.5">
      <div className="flex flex-wrap items-baseline gap-2">
        <span className="text-xs font-medium text-fg-default">
          {share.label?.trim() || share.id}
        </span>
        <span className="font-mono text-caption text-fg-subtle">
          category: {share.category}
        </span>
        {share.read_only && <Badge variant="info">read-only</Badge>}
        {disabled && (
          <Badge variant="warning">{revoked ? "revoked" : "expired"}</Badge>
        )}
        <span className="ml-auto text-caption text-fg-subtle">
          token …{share.token_last4}
        </span>
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 font-mono text-caption text-fg-subtle">
        <span className="truncate">{share.repo_url}#{share.repo_ref}</span>
        <span aria-hidden>·</span>
        <span className="truncate">{share.config_path}</span>
      </div>
      {(share.allowed_paths?.length ?? 0) > 0 && (
        <div className="mt-0.5 text-caption text-fg-subtle">
          fields: {share.allowed_paths.join(", ")}
        </div>
      )}
      <div className="mt-1 flex flex-wrap items-center gap-2 text-caption text-fg-subtle">
        <span>
          created {formatRelative(share.created_at)}
          {share.expires_at && !expired && (
            <> · expires {formatRelative(share.expires_at)}</>
          )}
          {!share.expires_at && !revoked && <> · never expires</>}
          {share.last_used_at && <> · last used {formatRelative(share.last_used_at)}</>}
        </span>
        <span className="ml-auto flex items-center gap-1.5">
          <Button variant="ghost" size="sm" onClick={onDeliveries} disabled={busy}>
            Deliveries
          </Button>
          <Button variant="secondary" size="sm" onClick={onRotate} disabled={busy}>
            Rotate token
          </Button>
          <Button variant="ghost" size="sm" onClick={onDelete} disabled={busy}>
            Revoke
          </Button>
        </span>
      </div>
    </li>
  );
}

// ---------------------------------------------------------------------------
// Create dialog
// ---------------------------------------------------------------------------

function CreateShareDialog({
  teamID,
  botID,
  spec,
  onCancel,
  onCreated,
}: {
  teamID: string;
  botID: string;
  spec: ConfigShareSpec;
  onCancel: () => void;
  onCreated: (minted: ShareWithToken) => void;
}) {
  const [label, setLabel] = useState("");
  const [repoURL, setRepoURL] = useState("");
  const [repoRef, setRepoRef] = useState("main");
  const [category, setCategory] = useState("");
  const [readOnly, setReadOnly] = useState(false);
  const [expiresDays, setExpiresDays] = useState<number | "">(14);
  const [neverExpires, setNeverExpires] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The bot DECLARES its shareable surface (spec); the server derives + pins
  // the exact editable/visible paths + config file at mint. This form only
  // collects who/where + the category + WHICH of the declared editable fields
  // this share exposes — never raw paths, which keeps it generic (a second bot
  // needs no change here).
  const needsCategory = useMemo(
    () =>
      [...spec.editable_paths, ...(spec.visible_paths ?? [])].some((p) =>
        p.includes("{category}"),
      ),
    [spec],
  );
  // Declared editable fields, by leaf name — the least-privilege subset unit.
  // Start with all selected (the full declared surface).
  const editableFields = useMemo(
    () => spec.editable_paths.map((p) => p.split(".").pop() ?? p),
    [spec],
  );
  const [selected, setSelected] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(editableFields.map((f) => [f, true])),
  );
  const selectedFields = editableFields.filter((f) => selected[f]);
  const expand = (p: string) =>
    p.split("{category}").join(category.trim() || "<category>");

  const submitDisabled =
    !label.trim() ||
    !repoURL.trim() ||
    !repoRef.trim() ||
    (needsCategory && !category.trim()) ||
    selectedFields.length === 0 ||
    busy;

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const input: CreateShareInput = {
        bot_id: botID,
        label: label.trim(),
        repo_url: repoURL.trim(),
        repo_ref: repoRef.trim(),
        category: category.trim(),
        // Least-privilege subset of the bot's declared editable fields; the
        // server derives + pins the paths. config_path + paths never sent.
        editable_fields: selectedFields,
        read_only: readOnly,
        ...(neverExpires
          ? { never_expires: true }
          : typeof expiresDays === "number" && expiresDays > 0
            ? { expires_days: expiresDays }
            : {}),
      };
      const minted = await createConfigShare(teamID, input);
      onCreated(minted);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v && !busy) onCancel();
      }}
      title="Create a config-share link"
      description="Pick which category and which fields the visitor may edit. The link + token are shown once after creation."
      widthClass="max-w-xl"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={() => void submit()}
            loading={busy}
            disabled={submitDisabled}
          >
            Mint share link
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-sm">
        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}
        <div>
          <FieldLabel htmlFor="cs-label">Label</FieldLabel>
          <Input
            id="cs-label"
            size="md"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="e.g. Veille A11y — Alice"
          />
        </div>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <FieldLabel htmlFor="cs-repo">Repo URL</FieldLabel>
            <Input
              id="cs-repo"
              size="md"
              value={repoURL}
              onChange={(e) => setRepoURL(e.target.value)}
              placeholder="https://github.com/org/repo"
              className="font-mono"
            />
          </div>
          <div>
            <FieldLabel htmlFor="cs-ref">Branch / ref</FieldLabel>
            <Input
              id="cs-ref"
              size="md"
              value={repoRef}
              onChange={(e) => setRepoRef(e.target.value)}
              placeholder="main"
              className="font-mono"
            />
          </div>
        </div>
        {needsCategory && (
          <div>
            <FieldLabel htmlFor="cs-cat">Category</FieldLabel>
            <Input
              id="cs-cat"
              size="md"
              value={category}
              onChange={(e) => setCategory(e.target.value)}
              placeholder="a11y"
              className="font-mono"
            />
          </div>
        )}
        {editableFields.length > 1 && (
          <div>
            <FieldLabel>Fields this share may edit</FieldLabel>
            <div className="flex flex-col gap-1 rounded-md border border-border-default bg-surface-2 p-2">
              {editableFields.map((f) => (
                <Checkbox
                  key={f}
                  checked={!!selected[f]}
                  onChange={(e) =>
                    setSelected((s) => ({ ...s, [f]: e.target.checked }))
                  }
                  label={f}
                />
              ))}
            </div>
            <p className="mt-1 text-caption text-fg-subtle">
              Uncheck a field to withhold it — a share can't touch or even read
              the fields you leave off.
            </p>
          </div>
        )}
        <div>
          <FieldLabel>What this share exposes</FieldLabel>
          <div className="rounded-md border border-border-default bg-surface-2 p-2 text-xs">
            <div className="mb-1 text-fg-subtle">
              Config file:{" "}
              <span className="font-mono text-fg-default">
                {spec.config_path}
              </span>
            </div>
            <div className="font-medium text-fg-default">Editable</div>
            <ul className="mb-1 flex flex-col gap-0.5">
              {spec.editable_paths
                .filter((p) => selected[p.split(".").pop() ?? p])
                .map((p) => (
                  <li key={p} className="font-mono text-caption text-fg-muted">
                    {expand(p)}
                  </li>
                ))}
            </ul>
            {(spec.visible_paths?.length ?? 0) > 0 && (
              <>
                <div className="font-medium text-fg-default">
                  Read-only context
                </div>
                <ul className="flex flex-col gap-0.5">
                  {(spec.visible_paths ?? []).map((p) => (
                    <li
                      key={p}
                      className="font-mono text-caption text-fg-subtle"
                    >
                      {expand(p)}
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>
          <p className="mt-1 text-caption text-fg-subtle">
            These fields come from the bot's manifest and can't be widened here.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-4">
          <Checkbox
            checked={readOnly}
            onChange={(e) => setReadOnly(e.target.checked)}
            label="Read-only share"
          />
          <Checkbox
            checked={neverExpires}
            onChange={(e) => setNeverExpires(e.target.checked)}
            label="Never expires"
          />
          <div
            className={`flex items-center gap-2 text-xs ${neverExpires ? "opacity-50" : ""}`}
          >
            <label htmlFor="cs-exp">Expires in</label>
            <Input
              id="cs-exp"
              type="number"
              min={1}
              size="sm"
              disabled={neverExpires}
              value={neverExpires ? "" : expiresDays}
              onChange={(e) => {
                const n = e.target.value === "" ? "" : Number(e.target.value);
                setExpiresDays(Number.isFinite(n) ? (n as number) : "");
              }}
              className="w-16 font-mono"
            />
            <span className="text-fg-subtle">days</span>
          </div>
        </div>
      </div>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Token-once dialog — the ONE moment the plaintext token is on screen.
// ---------------------------------------------------------------------------

function TokenOnceDialog({
  minted,
  onClose,
}: {
  minted: ShareWithToken;
  onClose: () => void;
}) {
  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title="Share link — copy it now"
      description="The token is shown only this once. If you lose it, rotate the share to mint a fresh one."
      widthClass="max-w-2xl"
      stack="confirm"
      footer={
        <Button variant="primary" size="sm" onClick={onClose}>
          Done — hide token
        </Button>
      }
    >
      <div className="space-y-3 text-sm">
        <section>
          <div className="text-xs uppercase tracking-wider text-fg-muted">
            Complete link (URL + token)
          </div>
          <div className="flex items-center gap-2 rounded border border-border-subtle bg-surface-0 p-2 font-mono text-xs break-all">
            <span className="flex-1" data-testid="config-share-url">
              {minted.url}
            </span>
            <CopyButton value={minted.url} variant="icon" />
          </div>
          <p className="mt-1 text-caption text-fg-subtle">
            Send this whole URL to the editor. The part after{" "}
            <code className="font-mono">#</code> is the credential — chat apps
            that strip fragments will break the link.
          </p>
        </section>
        <section>
          <div className="text-xs uppercase tracking-wider text-fg-muted">
            Token alone
          </div>
          <div className="flex items-center gap-2 rounded border border-border-subtle bg-surface-0 p-2 font-mono text-xs break-all">
            <span className="flex-1">{minted.token}</span>
            <CopyButton value={minted.token} variant="icon" />
          </div>
        </section>
        <InlineBanner tone="warning" layout="inline">
          Once you close this dialog, the token is gone from this session. Only
          the last 4 characters (…{minted.token_last4}) remain visible for
          identification.
        </InlineBanner>
      </div>
    </Dialog>
  );
}
