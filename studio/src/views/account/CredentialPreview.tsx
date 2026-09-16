import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { listBots } from "@/api/bots";
import { listWebhooks } from "@/api/webhooks";
import { previewCredentials, type CredentialPreviewCandidate, type CredentialPreviewRequest } from "@/api/credentialPreview";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Select } from "@/components/ui/Select";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";

const tierNames: Record<string, string> = {
  user: "Personal", team: "Team", org: "Organization", pool: "Shared pool", platform: "Platform",
};

export default function CredentialPreview({ teamID }: { teamID: string }) {
  return <ScopedCredentialPreview key={teamID} teamID={teamID} />;
}

function ScopedCredentialPreview({ teamID }: { teamID: string }) {
  const [source, setSource] = useState("personal");
  const [botID, setBotID] = useState("");
  const [webhookID, setWebhookID] = useState("");
  const bots = useQuery({ queryKey: ["credential-preview-bots", teamID], queryFn: listBots });
  const hooks = useQuery({ queryKey: ["credential-preview-webhooks", teamID], queryFn: () => listWebhooks(teamID), enabled: source === "webhook" });
  const input: CredentialPreviewRequest = {
    source: source === "webhook" ? { kind: source, id: webhookID } : { kind: source },
    ...(botID ? { bot_id: botID } : {}),
  };
  const preview = useMutation({ mutationFn: (request: CredentialPreviewRequest) => previewCredentials(teamID, request) });
  // A result describes one source and bot. Editing either immediately hides
  // the old observation, including when its request completes afterwards.
  const sameInput = JSON.stringify(preview.variables) === JSON.stringify(input);
  const result = sameInput ? preview.data : undefined;
  const selectedHook = hooks.data?.find((hook) => hook.id === webhookID);
  const visibleBots = (bots.data ?? []).filter((bot) => source !== "webhook" || !selectedHook || selectedHook.wildcard_bots || (selectedHook.bot_ids ?? []).includes("*") || (selectedHook.bot_ids ?? []).includes(bot.name));
  const ready = source === "personal" ? !!botID : !!webhookID;
  const fetchError = bots.error ?? (source === "webhook" ? hooks.error : null);

  return <section className="space-y-3 rounded border border-border-subtle bg-surface-1 p-4">
    <div>
      <h3 className="font-medium">Which account would fund this run?</h3>
      <p className="mt-1 text-sm text-fg-muted">See the credential order and the observed conditions for falling back. Capacity is checked again when a run starts.</p>
    </div>
    <form className="flex flex-wrap items-end gap-3" onSubmit={(event) => { event.preventDefault(); if (ready) preview.mutate(input); }}>
      <label className="min-w-44 text-xs text-fg-muted">Launch source
        <Select aria-label="Credential preview launch source" value={source} onChange={(event) => { setSource(event.target.value); setBotID(""); }}>
          <option value="personal">Launched by me</option>
          <option value="webhook">An existing webhook</option>
        </Select>
      </label>
      {source === "webhook" && <label className="min-w-44 text-xs text-fg-muted">Webhook
        <Select aria-label="Credential preview webhook" value={webhookID} onChange={(event) => { setWebhookID(event.target.value); setBotID(""); }}>
          <option value="">Choose a webhook</option>
          {(hooks.data ?? []).map((hook) => <option key={hook.id} value={hook.id}>{hook.name}{hook.enabled ? "" : " (disabled)"}</option>)}
        </Select>
      </label>}
      <label className="min-w-44 text-xs text-fg-muted">Bot
        <Select aria-label="Credential preview bot" value={botID} onChange={(event) => setBotID(event.target.value)}>
          <option value="">{source === "webhook" ? "Use the webhook default" : "Choose a bot"}</option>
          {visibleBots.map((bot) => <option key={bot.name} value={bot.name}>{bot.display_name ? `${bot.display_name} (${bot.name})` : bot.name}</option>)}
        </Select>
      </label>
      <Button type="submit" variant="secondary" disabled={!ready || preview.isPending} loading={preview.isPending}>Preview fallback chain</Button>
    </form>
    {fetchError && <InlineBanner tone="warning">{errorMessage(fetchError)}</InlineBanner>}
    {sameInput && preview.error && <InlineBanner tone="danger">{errorMessage(preview.error)}</InlineBanner>}
    {result && <div className="space-y-3">
      <p className="text-xs text-fg-muted">Observed {formatDateTime(result.observed_at)} · {result.context.bot_id} · {result.context.source.kind === "personal" ? "your personal launch" : "webhook launch"}</p>
      {(result.warnings ?? []).map((warning, index) => <InlineBanner key={index} tone="warning">{warning}</InlineBanner>)}
      <p className="text-xs text-fg-muted">Shared pool: {result.pool.considered ? "consulted" : "not consulted"}{result.pool.reason ? ` — ${result.pool.reason}` : ""}</p>
      {(result.candidates ?? []).length === 0 && <p className="text-sm text-fg-muted">No stored credential candidates in this observation.</p>}
      {[...new Set((result.candidates ?? []).map((candidate) => candidate.wire))].map((wire) => <div key={wire} className="space-y-2">
        <h4 className="text-sm font-medium">{wire}</h4>
        <ol className="space-y-2">
          {(result.candidates ?? []).filter((candidate) => candidate.wire === wire).map((candidate) => <Candidate key={candidate.id} candidate={candidate} shared={!!candidate.account_group && (result.candidates ?? []).filter((other) => other.account_group === candidate.account_group).length > 1} />)}
        </ol>
      </div>)}
    </div>}
  </section>;
}

function Candidate({ candidate: c, shared }: { candidate: CredentialPreviewCandidate; shared: boolean }) {
  const blocked = ["blocked", "window_closed", "at_capacity", "restored"].includes(c.state);
  return <li className="rounded border border-border-subtle p-3 text-sm space-y-1">
    <div className="flex flex-wrap items-center gap-2">
      <span className="font-medium">{tierNames[c.tier] ?? c.tier} · {c.label}</span>
      <Badge variant={blocked ? "warning" : c.selected && ["available", "active"].includes(c.state) ? "success" : "neutral"}>{c.state === "restored" ? "Restored to wait for quota" : c.state.replace(/_/g, " ")}</Badge>
      {c.selected && !blocked && <span className="text-xs text-fg-muted">Chosen if launch checks pass</span>}
      {c.selection === "shadowed" && <span className="text-xs text-fg-muted">Later candidate</span>}
      {c.selection === "not_consulted" && <span className="text-xs text-fg-muted">Not consulted for this launch</span>}
      {c.pinned && <Badge variant="neutral">Pinned</Badge>}
      {shared && <Badge variant="warning">Shared account quota</Badge>}
    </div>
    <div className="text-xs text-fg-muted">{c.provider} · {c.source === "oauth" ? (c.rank === 0 ? "Primary subscription" : `Subscription fallback ${c.rank}`) : "API key"}</div>
    {c.reason && <p className="text-xs text-fg-muted">{c.reason}</p>}
    {c.reopens_at && <p className="text-xs text-fg-muted">Reopens {formatDateTime(c.reopens_at)}</p>}
    {(c.windows ?? []).map((window, index) => <p key={index} className="text-xs text-fg-muted">
      {window.name.replace(/_/g, " ")}: {window.status === "rejected" && !window.percent ? "provider refused; usage unknown" : window.percent == null ? "usage unknown" : `${window.percent}% used`}
      {window.fresh ? "" : " (stale observation)"}{window.resets_at ? ` · resets ${formatDateTime(window.resets_at)}` : ""}
    </p>)}
    {c.capacity && <p className="text-xs text-fg-muted">
      {c.capacity.live_runs == null ? "Concurrent use unknown" : `${c.capacity.live_runs} active run(s)`}
      {c.capacity.max_concurrent_runs ? ` / ${c.capacity.max_concurrent_runs} allowed` : ""}
      {c.capacity.remaining_usd == null ? "" : ` · allowance for this run: $${c.capacity.remaining_usd.toFixed(2)}`}
    </p>}
  </li>;
}
