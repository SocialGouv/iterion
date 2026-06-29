import { useCallback, useEffect, useState } from "react";

import { listPlugins, setPluginEnabled, type PluginView } from "@/api/plugins";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";

// Plugins lists the iterion plugin registry (embedded builtins +
// ~/.iterion/plugins) and lets the operator enable/disable each one.
// Backed by GET /api/v1/plugins + POST /api/v1/plugins/{name}/{enable,disable}.
export default function Plugins() {
  const [plugins, setPlugins] = useState<PluginView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setError(null);
      setPlugins(await listPlugins());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const toggle = useCallback(
    async (p: PluginView) => {
      setBusy(p.name);
      try {
        await setPluginEnabled(p.name, !p.enabled);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(null);
      }
    },
    [refresh],
  );

  return (
    <div className="mx-auto max-w-3xl p-6">
      <header className="mb-4">
        <h1 className="text-lg font-medium text-fg-default">Plugins</h1>
        <p className="text-caption text-fg-subtle">
          Extend iterion with rewriters, MCP servers, skills, commands, agents
          and hooks. Builtins ship with the binary; install more with{" "}
          <code>iterion plugin install</code>. See docs/plugins.md.
        </p>
      </header>

      {error && (
        <div className="mb-4">
          <InlineBanner tone="danger" title="Plugin error" layout="inline">
            {error}
          </InlineBanner>
        </div>
      )}

      {plugins === null && !error && (
        <EmptyState message="Loading plugins…" icon={<Spinner />} />
      )}

      {plugins !== null && plugins.length === 0 && (
        <EmptyState message="No plugins found." />
      )}

      <ul className="flex flex-col gap-2">
        {(plugins ?? []).map((p) => (
          <li
            key={p.name}
            className="flex items-start justify-between gap-4 rounded-md border border-border-default bg-surface-1 p-3"
          >
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <span className="font-mono text-sm text-fg-default">{p.name}</span>
                {p.version && (
                  <span className="text-caption text-fg-subtle">{p.version}</span>
                )}
                <Badge variant={p.builtin ? "info" : "neutral"} size="sm">
                  {p.builtin ? "builtin" : "installed"}
                </Badge>
                {p.enabled && (
                  <Badge variant="success" size="sm">
                    enabled
                  </Badge>
                )}
              </div>
              {p.description && (
                <p className="mt-1 text-caption text-fg-muted">{p.description}</p>
              )}
              <div className="mt-1.5 flex flex-wrap gap-1">
                {p.kinds.map((k) => (
                  <Badge key={k} variant="accent" size="sm">
                    {k}
                  </Badge>
                ))}
              </div>
            </div>
            <Button
              variant={p.enabled ? "secondary" : "primary"}
              size="sm"
              loading={busy === p.name}
              disabled={busy !== null}
              onClick={() => void toggle(p)}
            >
              {p.enabled ? "Disable" : "Enable"}
            </Button>
          </li>
        ))}
      </ul>
    </div>
  );
}
