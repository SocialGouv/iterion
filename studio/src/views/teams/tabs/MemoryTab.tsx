import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { clickableRowProps } from "@/lib/a11y";
import { formatDateTime } from "@/lib/format";

import {
  FeatureUnavailableError,
  type MemoryDocumentMeta,
  type MemorySpaceRef,
  type MemoryVisibility,
  getMemoryUsage,
  listMemoryDocuments,
  memoryExportURL,
  readMemoryDocument,
} from "@/api/memory";
import { fmtBytes, pct } from "@/api/usage";

import { Button } from "@/components/ui/Button";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Spinner } from "@/components/ui/Spinner";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useBotsStore } from "@/store/bots";

interface Props {
  teamID: string;
}

// All memory visibilities the server resolves. Some require extra
// qualifiers — we only expose those that make sense from a TeamPage:
// org-wide spaces by default, plus the global catalogue when the user
// wants to peek.
const VISIBILITIES: Array<{ id: MemoryVisibility; label: string; needsBot?: boolean }> = [
  { id: "org", label: "Org (shared across all bots)" },
  { id: "project", label: "Project (this workspace)" },
  { id: "bot", label: "Bot (per-bot scratch)", needsBot: true },
  { id: "user", label: "User (just me)" },
  { id: "global", label: "Global (instance-wide, read-only)" },
];

export default function MemoryTab({ teamID: _teamID }: Props) {
  const [ref, setRef] = useState<MemorySpaceRef>({ name: "default", visibility: "org" });
  const [usage, setUsage] = useState<{ used: number; quota: number } | null>(null);
  const [docs, setDocs] = useState<MemoryDocumentMeta[]>([]);
  const [selected, setSelected] = useState<MemoryDocumentMeta | null>(null);
  const [body, setBody] = useState<string>("");
  const [unavailable, setUnavailable] = useState(false);
  // Bots come from the shared catalog cache so the picker matches every
  // other bot-scoped surface (Bindings, Webhooks). freeSolo keeps
  // typed-only ids (a bot not in the catalog, or an obsolete slug) usable.
  const bots = useBotsStore((s) => s.bots) ?? [];
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);
  const botOptions: ComboboxOption[] = bots.map((b) => ({
    value: b.name,
    label: b.name,
    description: b.display_name && b.display_name !== b.name ? b.display_name : undefined,
    searchHaystack: `${b.name} ${b.display_name ?? ""}`,
  }));
  // Single busy/error channel for both the list-load and the document
  // open — Load button mirrors `busy`, banner above mirrors `error`.
  // setError handles the pre-flight "needs a bot id" guard without
  // invoking run() (so busy doesn't flip on for a validation error).
  const { busy: loading, error: err, run, setError } = useAsyncAction();

  const reload = () => {
    if (ref.visibility === "bot" && !ref.bot) {
      setError("This visibility needs a bot id.");
      return;
    }
    void run(async () => {
      try {
        const [u, d] = await Promise.all([
          getMemoryUsage(ref).then((x) => ({ used: x.used_bytes, quota: x.quota_bytes })),
          listMemoryDocuments(ref),
        ]);
        setUsage(u);
        setDocs(d);
        setSelected(null);
        setBody("");
        setUnavailable(false);
      } catch (e) {
        if (e instanceof FeatureUnavailableError) {
          setUnavailable(true);
          return;
        }
        throw e;
      }
    });
  };

  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const open = (d: MemoryDocumentMeta) => {
    setSelected(d);
    setBody("");
    void run(async () => {
      const txt = await readMemoryDocument(ref, d.path);
      setBody(txt);
    });
  };

  if (unavailable) {
    return (
      <EmptyState
        title="Memory browser not enabled on this server"
        message="The shared-memory REST surface requires the cloud-mode knowledge store."
      />
    );
  }

  const cur = VISIBILITIES.find((v) => v.id === ref.visibility);

  return (
    <div className="space-y-4">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      <section className="grid grid-cols-1 sm:grid-cols-4 gap-2 items-end">
        <label className="block text-xs">
          <div className="text-fg-muted mb-1">Visibility</div>
          <Select
            value={ref.visibility}
            onChange={(e) =>
              setRef((r) => ({ ...r, visibility: e.target.value as MemoryVisibility }))
            }
          >
            {VISIBILITIES.map((v) => (
              <option key={v.id} value={v.id}>
                {v.label}
              </option>
            ))}
          </Select>
        </label>
        <label className="block text-xs">
          <div className="text-fg-muted mb-1">Space name</div>
          <Input
            value={ref.name}
            onChange={(e) => setRef((r) => ({ ...r, name: e.target.value }))}
            placeholder="default"
          />
        </label>
        {cur?.needsBot && (
          <label className="block text-xs">
            <div className="text-fg-muted mb-1">Bot id</div>
            <Combobox
              value={ref.bot ?? ""}
              options={botOptions}
              onChange={(v) => setRef((r) => ({ ...r, bot: v || undefined }))}
              placeholder="Pick or type a bot id"
              freeSolo
            />
          </label>
        )}
        <label className="block text-xs">
          <div className="text-fg-muted mb-1">Project (optional)</div>
          <Input
            value={ref.project ?? ""}
            onChange={(e) => setRef((r) => ({ ...r, project: e.target.value }))}
            placeholder=""
          />
        </label>
      </section>

      <div className="flex gap-2">
        <Button size="sm" variant="primary" loading={loading} onClick={() => void reload()}>
          Load
        </Button>
        <a
          href={memoryExportURL(ref)}
          className="inline-flex items-center text-xs text-fg-muted hover:text-fg-default underline"
        >
          Export space (.tar.gz)
        </a>
      </div>

      {usage && (
        <div className="text-xs space-y-1 bg-surface-1 border border-border-subtle rounded p-3">
          <div className="text-fg-muted">Storage</div>
          <div className="flex items-baseline gap-1">
            <span className="text-lg font-semibold">{fmtBytes(usage.used)}</span>
            {usage.quota > 0 && (
              <span className="text-fg-muted">/ {fmtBytes(usage.quota)}</span>
            )}
          </div>
          {usage.quota > 0 && (
            <div
              className="h-1.5 bg-surface-2 rounded overflow-hidden"
              role="progressbar"
              aria-valuenow={Math.round(pct(usage.used, usage.quota) ?? 0)}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-label="Storage usage"
            >
              <div
                className="h-full bg-accent"
                style={{ width: `${pct(usage.used, usage.quota) ?? 0}%` }}
              />
            </div>
          )}
        </div>
      )}

      <section className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <div>
          <h3 className="font-medium mb-2">Documents</h3>
          {docs.length === 0 ? (
            <EmptyState message="No documents in this space yet." />
          ) : (
            <ul className="divide-y divide-border-subtle bg-surface-1 border border-border-subtle rounded">
              {docs.map((d) => (
                <li
                  key={d.path}
                  className={`px-2 py-1.5 text-sm cursor-pointer hover:bg-surface-2 focus-visible:bg-surface-2 focus-visible:outline-none ${
                    selected?.path === d.path ? "bg-surface-2" : ""
                  }`}
                  {...clickableRowProps(() => void open(d), `Open ${d.path}`)}
                >
                  <div className="font-mono text-xs">{d.path}</div>
                  <div className="text-xs text-fg-muted">
                    {fmtBytes(d.size)} ·{" "}
                    {d.updated_at ? formatDateTime(d.updated_at) : ""}
                  </div>
                </li>
              ))}
            </ul>
          )}
        </div>
        <div>
          <h3 className="font-medium mb-2">
            Preview {selected ? <span className="font-mono text-xs">{selected.path}</span> : null}
          </h3>
          {selected ? (
            body ? (
              <pre className="bg-surface-1 border border-border-subtle rounded p-3 text-xs whitespace-pre-wrap break-words max-h-[60vh] overflow-auto">
                {body}
              </pre>
            ) : (
              <div className="flex items-center justify-center rounded border border-border-subtle bg-surface-1 p-6">
                <Spinner size="sm" label="Loading document" />
              </div>
            )
          ) : (
            <EmptyState message="Pick a document on the left to preview it." />
          )}
        </div>
      </section>
    </div>
  );
}
