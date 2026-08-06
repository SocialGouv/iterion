// Extracted from LaunchView.tsx to keep that file focused.
// RunSettingsSection renders the "Run settings" block: per-run backend
// override and compression override. Both selects are presentational —
// LaunchView owns the override state and feeds it to createRun().

import type { BackendDetectReport } from "@/api/backends";
import type {
  PreviewEffectiveKnob,
  PreviewEffectiveSettings,
} from "@/api/runs";

import { Select } from "@/components/ui/Select";

export interface RunSettingsSectionProps {
  backendOverride: string;
  compressOverride: string;
  autoMemoryOverride: string;
  permissionOverride: string;
  reviewModeOverride: string;
  backendReport: BackendDetectReport | null;
  // effective is the server-resolved provenance BELOW run-override
  // (workflow/env/default, from POST /api/runs/preview-cost). The
  // caption layers the local override on top.
  effective?: PreviewEffectiveSettings | null;
  onBackendChange: (value: string) => void;
  onCompressChange: (value: string) => void;
  onAutoMemoryChange: (value: string) => void;
  onPermissionChange: (value: string) => void;
  onReviewModeChange: (value: string) => void;
  /** True when the bot's parsed doc declares a `review_mode` var — the
   *  hook the resolver's InjectIfDeclared inspects server-side. Bots
   *  that don't declare one silently ignore an override, so we hide
   *  the picker instead of offering a no-op. Permission stays visible
   *  regardless: CLI `--permission` overrides apply to any workflow. */
  showReviewMode: boolean;
}

// knobCaption renders "effective: X · from Y" — the override wins when
// the operator set the select; else the server's workflow/env/default
// resolution, plus a node-pinned warning when a run override wouldn't
// reach every node.
function knobCaption(override: string, knob: PreviewEffectiveKnob | undefined) {
  if (!override && !knob) return null;
  const effective = override || knob?.effective || "";
  const source = override ? "run override" : (knob?.source ?? "");
  return (
    <div className="mt-1 text-caption text-fg-subtle">
      effective: <code>{effective}</code>
      {source ? <> · from {source}</> : null}
      {knob?.node_pinned ? (
        <> · some nodes pin their own (override won’t affect them)</>
      ) : null}
    </div>
  );
}

export default function RunSettingsSection({
  backendOverride,
  compressOverride,
  autoMemoryOverride,
  permissionOverride,
  reviewModeOverride,
  backendReport,
  effective,
  onBackendChange,
  onCompressChange,
  onAutoMemoryChange,
  onPermissionChange,
  onReviewModeChange,
  showReviewMode,
}: RunSettingsSectionProps) {
  return (
    <section className="mt-6 border-t border-border-default pt-4 mb-6">
      <h2 className="text-xs font-medium text-fg-muted mb-3">Run settings</h2>
      <div className="space-y-4">
        <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
          <div>
            <div className="text-xs font-medium font-mono">backend</div>
            <div className="text-caption text-fg-subtle">override for this run</div>
          </div>
          <div>
            <Select
              value={backendOverride}
              onChange={(e) => onBackendChange(e.currentTarget.value)}
            >
              <option value="">
                auto{backendReport?.resolved_default
                  ? ` — currently ${backendReport.resolved_default}`
                  : ""}
              </option>
              {(backendReport?.backends ?? []).map((b) => (
                <option
                  key={b.name}
                  value={b.name}
                  disabled={!b.available}
                >
                  {b.name}
                  {b.available
                    ? b.auth !== "none"
                      ? ` (${b.auth})`
                      : ""
                    : " — no credential"}
                </option>
              ))}
            </Select>
            {knobCaption(backendOverride, effective?.backend)}
            <div className="mt-1 text-caption text-fg-subtle">
              Overrides the workflow&apos;s default. Nodes that pin a specific{" "}
              <code>backend:</code> keep their pin.
            </div>
          </div>
        </div>
        <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
          <div>
            <div className="text-xs font-medium font-mono">compress</div>
            <div className="text-caption text-fg-subtle">output compression</div>
          </div>
          <div>
            <Select
              value={compressOverride}
              onChange={(e) => onCompressChange(e.currentTarget.value)}
            >
              <option value="">default — on when a rewriter plugin is enabled</option>
              <option value="on">on — compress shell output</option>
              <option value="ultra">ultra — densest output</option>
              <option value="off">off — disable for this run</option>
            </Select>
            {knobCaption(compressOverride, effective?.compress)}
            <div className="mt-1 text-caption text-fg-subtle">
              Rewrites agent shell commands via the active rewriter plugin (
              <a
                href="https://github.com/rtk-ai/rtk"
                target="_blank"
                rel="noreferrer"
                className="underline"
              >
                rtk
              </a>{" "}
              by default) to save 60–90% of command-output tokens. On agent/judge
              nodes it&apos;s on by default when a rewriter plugin is enabled
              (its binary on the host PATH); choose <code>off</code> to disable
              for this run. Tool nodes stay opt-in via the bot&apos;s{" "}
              <code>compress:</code> field.
            </div>
          </div>
        </div>
        <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
          <div>
            <div className="text-xs font-medium font-mono">auto_memory</div>
            <div className="text-caption text-fg-subtle">MEMORY.md</div>
          </div>
          <div>
            <Select
              value={autoMemoryOverride}
              onChange={(e) => onAutoMemoryChange(e.currentTarget.value)}
            >
              <option value="">default — off unless the bot asks for it</option>
              <option value="on">on — carry memory across runs</option>
              <option value="off">off — hermetic run</option>
            </Select>
            {knobCaption(autoMemoryOverride, effective?.auto_memory)}
            <div className="mt-1 text-caption text-fg-subtle">
              Lets agent/judge nodes read and maintain a persistent{" "}
              <code>MEMORY.md</code> shared by every run of this bot on this
              project — what it learned, where it left off. Off by default so a
              run stays hermetic. Honoured by <code>claude_code</code>,{" "}
              <code>claw</code> and <code>pi</code>.
            </div>
          </div>
        </div>
        <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
          <div>
            <div className="text-xs font-medium font-mono">permission</div>
            <div className="text-caption text-fg-subtle">tool-use gate</div>
          </div>
          <div>
            <Select
              value={permissionOverride}
              onChange={(e) => onPermissionChange(e.currentTarget.value)}
            >
              <option value="">inherit (workflow / ITERION_PERMISSION)</option>
              <option value="ask">ask — pause for approval on off-policy tool use</option>
              <option value="deny">deny — hard-block off-policy tool use (headless)</option>
              <option value="off">off — no gate (default)</option>
            </Select>
            {knobCaption(permissionOverride, effective?.permission)}
            <div className="mt-1 text-caption text-fg-subtle">
              Anti-prompt-injection gate: tool calls outside the workflow&apos;s{" "}
              <code>allow:</code> list are paused for your approval (
              <code>ask</code>) or blocked (<code>deny</code>). Rule lists are
              set in the <code>.bot</code> DSL. See docs/permissions.md.
            </div>
          </div>
        </div>
        {showReviewMode && (
          <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
            <div>
              <div className="text-xs font-medium font-mono">review_mode</div>
              <div className="text-caption text-fg-subtle">review topology</div>
            </div>
            <div>
              <Select
                value={reviewModeOverride}
                onChange={(e) => onReviewModeChange(e.currentTarget.value)}
              >
                <option value="">auto — resolve from detected providers</option>
                <option value="mono">mono — single-family review</option>
                <option value="dual">dual — cross-family review</option>
              </Select>
              <div className="mt-1 text-caption text-fg-subtle">
                Mono runs the review side against a single provider family;
                dual splits reviewers across two families for cross-check.
              </div>
            </div>
          </div>
        )}
      </div>
    </section>
  );
}
