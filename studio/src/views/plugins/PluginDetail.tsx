import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";

import {
  getPluginDetail,
  runPluginLifecycle,
  type PluginDetail as PluginDetailData,
  type PluginLifecycleResult,
  type PluginView,
} from "@/api/plugins";
import { errorMessage } from "@/lib/errorHints";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Drawer } from "@/components/ui/Drawer";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import PluginConfigForm from "./PluginConfigForm";

interface Props {
  /** Live view from the registry list — stays fresh across enable/disable
   *  refreshes (the fetched detail is only used for the deep sections). */
  plugin: PluginView;
  canManage: boolean;
  busy: boolean;
  onToggle: () => void;
  onRemove: () => void;
  onClose: () => void;
  onConfigSaved: () => void;
}

/** PluginDetail is the right-side drawer that opens when the operator clicks
 *  a plugin card (modeled on MarketplaceDetail). It fetches the full detail
 *  projection (contributes, hooks, lifecycle, README) so the operator can vet
 *  everything a plugin does — hook shell commands are surfaced in a warning
 *  banner BEFORE the Enable action — and hosts the config form inline. */
export default function PluginDetail({
  plugin,
  canManage,
  busy,
  onToggle,
  onRemove,
  onClose,
  onConfigSaved,
}: Props) {
  const detailQuery = useQuery<PluginDetailData>({
    queryKey: ["plugin-detail", plugin.name],
    queryFn: () => getPluginDetail(plugin.name),
  });
  // isFetching (not isLoading) so a reopen shows the loading state until
  // the fresh detail lands — every load is a visible reload, and the
  // loading / loaded / errored states stay mutually exclusive as before.
  const detailLoading = detailQuery.isFetching;
  const detail =
    detailLoading || detailQuery.error ? null : (detailQuery.data ?? null);
  const detailErr =
    detailQuery.error && !detailLoading ? errorMessage(detailQuery.error) : null;

  const configurable = canManage && (plugin.config_schema?.length ?? 0) > 0;

  return (
    <Drawer
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={<span className="block truncate font-mono">{plugin.name}</span>}
      description={
        <span className="block truncate">
          {plugin.version ? `v${plugin.version} · ` : ""}
          {plugin.builtin ? "builtin" : "installed"}
        </span>
      }
      footer={
        <>
          <span className="mr-auto text-caption text-fg-subtle">
            {plugin.builtin
              ? "Builtins ship with the binary — disable, don't remove."
              : "Installed under ~/.iterion/plugins."}
          </span>
          {canManage && !plugin.builtin && (
            <Button
              variant="danger"
              size="sm"
              loading={busy}
              disabled={busy}
              onClick={onRemove}
            >
              Remove
            </Button>
          )}
          {canManage ? (
            <Button
              variant={plugin.enabled ? "secondary" : "primary"}
              size="sm"
              loading={busy}
              disabled={busy}
              onClick={onToggle}
            >
              {plugin.enabled ? "Disable" : "Enable"}
            </Button>
          ) : (
            <span className="text-caption text-fg-subtle">
              {plugin.enabled ? "Enabled" : "Disabled"} by admin
            </span>
          )}
        </>
      }
    >
      {/* About — from the live list view + fetched detail (origin dir). */}
      <section className="space-y-2 text-xs text-fg-default">
        {plugin.description && <p className="text-fg-muted">{plugin.description}</p>}
        <div className="flex flex-wrap items-center gap-1">
          <Badge variant={plugin.builtin ? "info" : "neutral"} size="sm">
            {plugin.builtin ? "builtin" : "installed"}
          </Badge>
          {plugin.enabled && (
            <Badge variant="success" size="sm">
              enabled
            </Badge>
          )}
          {plugin.kinds.map((k) => (
            <Badge key={k} variant="accent" size="sm">
              {k}
            </Badge>
          ))}
        </div>
        <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-micro text-fg-muted">
          {plugin.version && (
            <>
              <dt className="text-fg-subtle">Version</dt>
              <dd className="text-fg-default">{plugin.version}</dd>
            </>
          )}
          {plugin.author && (
            <>
              <dt className="text-fg-subtle">Author</dt>
              <dd className="truncate text-fg-default">{plugin.author}</dd>
            </>
          )}
          <dt className="text-fg-subtle">Origin</dt>
          <dd className="text-fg-default">
            {plugin.builtin ? "builtin (embedded in the binary)" : "installed"}
          </dd>
          {detail?.dir && (
            <>
              <dt className="text-fg-subtle">Directory</dt>
              <dd className="truncate font-mono text-fg-default">{detail.dir}</dd>
            </>
          )}
        </dl>
      </section>

      {detailErr && (
        <div className="mt-4">
          <InlineBanner tone="danger" title="Failed to load plugin detail" layout="inline">
            {detailErr}
          </InlineBanner>
        </div>
      )}
      {detail === null && !detailErr && (
        <EmptyState message="Loading plugin detail…" icon={<Spinner />} className="py-6" />
      )}

      {detail && (
        <>
          <Contributes detail={detail} />

          {configurable && (
            <Section title="Configuration">
              <PluginConfigForm plugin={plugin} onSaved={onConfigSaved} />
            </Section>
          )}

          {detail.lifecycle && (detail.lifecycle.index || detail.lifecycle.refresh) && (
            <LifecycleSection
              name={plugin.name}
              detail={detail}
              canManage={canManage}
            />
          )}

          {detail.readme && (
            <Section title="README">
              <div className="max-h-96 overflow-y-auto rounded border border-border-default bg-surface-2 p-3">
                <MarkdownText value={detail.readme} size="sm" />
              </div>
            </Section>
          )}
        </>
      )}
    </Drawer>
  );
}

function Section({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <section className="mt-4 space-y-1.5">
      <h3 className="text-caption uppercase tracking-wide text-fg-subtle">{title}</h3>
      {children}
    </section>
  );
}

// Contributes lists everything the plugin wires into iterion. Hooks get a
// warning-toned banner with the raw shell commands so the operator gives
// informed consent before enabling.
function Contributes({ detail }: { detail: PluginDetailData }) {
  const rewriters = detail.rewriters ?? [];
  const mcp = detail.mcp_servers ?? [];
  const hooks = detail.hooks ?? [];
  const nameLists: [string, string[]][] = [
    ["Skills", detail.skills ?? []],
    ["Commands", detail.commands ?? []],
    ["Agents", detail.agents ?? []],
  ];
  const empty =
    rewriters.length === 0 &&
    mcp.length === 0 &&
    hooks.length === 0 &&
    nameLists.every(([, l]) => l.length === 0);
  if (empty) return null;

  return (
    <Section title="Contributes">
      {rewriters.length > 0 && (
        <ul className="space-y-1">
          {rewriters.map((r) => (
            <li
              key={r.id}
              className="rounded border border-border-default bg-surface-2 p-2 text-xs"
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="truncate font-medium text-fg-default">{r.id}</span>
                <span className="shrink-0 text-caption text-fg-subtle">rewriter</span>
              </div>
              {r.argv && r.argv.length > 0 && (
                <p className="mt-0.5 break-all font-mono text-micro text-fg-muted">
                  {r.argv.join(" ")}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
      {mcp.length > 0 && (
        <ul className="space-y-1">
          {mcp.map((s) => (
            <li
              key={s.name}
              className="rounded border border-border-default bg-surface-2 p-2 text-xs"
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="truncate font-medium text-fg-default">{s.name}</span>
                <span className="shrink-0 text-caption text-fg-subtle">
                  MCP · {s.transport}
                </span>
              </div>
              <p className="mt-0.5 break-all font-mono text-micro text-fg-muted">
                {s.url ?? [s.command, ...(s.args ?? [])].filter(Boolean).join(" ")}
              </p>
            </li>
          ))}
        </ul>
      )}
      {nameLists.map(
        ([label, items]) =>
          items.length > 0 && (
            <div key={label} className="text-xs">
              <span className="text-caption text-fg-subtle">{label}:</span>
              <span className="ml-1.5 inline-flex flex-wrap gap-1 align-middle">
                {items.map((n) => (
                  <span
                    key={n}
                    className="rounded bg-surface-2 px-1.5 py-0.5 text-caption text-fg-muted"
                  >
                    {n}
                  </span>
                ))}
              </span>
            </div>
          ),
      )}
      {hooks.length > 0 && (
        <InlineBanner
          tone="warning"
          layout="inline"
          title="Runs these shell commands on tool events"
        >
          <ul className="mt-1 space-y-1">
            {hooks.map((h) => (
              <li key={h.event}>
                <span className="font-medium">{h.event}</span>
                {(h.commands ?? []).map((c, i) => (
                  <pre
                    key={i}
                    className="mt-0.5 overflow-x-auto whitespace-pre-wrap break-all rounded bg-surface-1/60 p-1.5 font-mono text-micro"
                  >
                    {c}
                  </pre>
                ))}
              </li>
            ))}
          </ul>
        </InlineBanner>
      )}
    </Section>
  );
}

// LifecycleSection exposes the manifest's index/refresh commands as one-click
// runs (super-admin only server-side, mirrored by canManage here) and streams
// the command output live — including failures and truncation — verbatim.
function LifecycleSection({
  name,
  detail,
  canManage,
}: {
  name: string;
  detail: PluginDetailData;
  canManage: boolean;
}) {
  const [running, setRunning] = useState<"index" | "refresh" | null>(null);
  const [result, setResult] = useState<PluginLifecycleResult | null>(null);
  const [liveOutput, setLiveOutput] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const run = async (phase: "index" | "refresh") => {
    setRunning(phase);
    setErr(null);
    setResult(null);
    setLiveOutput("");
    try {
      setResult(await runPluginLifecycle(name, phase, setLiveOutput));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setRunning(null);
    }
  };

  return (
    <Section
      title={
        <span className="inline-flex items-center gap-1.5">
          Lifecycle
          {detail.auto_index && (
            <Badge variant="info" size="sm" className="normal-case tracking-normal">
              auto-index
            </Badge>
          )}
        </span>
      }
    >
      <div className="space-y-1 text-xs">
        {detail.lifecycle?.index && (
          <p className="break-all font-mono text-micro text-fg-muted">
            index: {detail.lifecycle.index}
          </p>
        )}
        {detail.lifecycle?.refresh && (
          <p className="break-all font-mono text-micro text-fg-muted">
            refresh: {detail.lifecycle.refresh}
          </p>
        )}
      </div>
      {canManage && (
        <div className="flex items-center gap-2">
          {detail.lifecycle?.index && (
            <Button
              variant="secondary"
              size="sm"
              loading={running === "index"}
              disabled={running !== null}
              onClick={() => void run("index")}
            >
              Run index
            </Button>
          )}
          {detail.lifecycle?.refresh && (
            <Button
              variant="secondary"
              size="sm"
              loading={running === "refresh"}
              disabled={running !== null}
              onClick={() => void run("refresh")}
            >
              Run refresh
            </Button>
          )}
        </div>
      )}
      {err && (
        <InlineBanner tone="danger" title="Lifecycle run failed" layout="inline">
          {err}
        </InlineBanner>
      )}
      {running !== null && liveOutput !== "" && (
        <pre
          aria-live="polite"
          className="max-h-64 overflow-auto rounded border border-border-default bg-surface-2 p-2 font-mono text-micro text-fg-default"
        >
          {liveOutput}
        </pre>
      )}
      {result && (
        <div className="space-y-1">
          {!result.ok && (
            <InlineBanner
              tone="danger"
              title={`${result.phase} command failed`}
              layout="inline"
            >
              {result.error}
            </InlineBanner>
          )}
          {result.output !== "" && (
            <pre className="max-h-64 overflow-auto rounded border border-border-default bg-surface-2 p-2 font-mono text-micro text-fg-default">
              {result.output}
            </pre>
          )}
          {result.ok && result.output === "" && (
            <p className="text-caption text-fg-subtle">
              {result.phase} completed with no output.
            </p>
          )}
          {result.truncated && (
            <p className="text-caption text-fg-subtle">Output truncated.</p>
          )}
        </div>
      )}
    </Section>
  );
}
