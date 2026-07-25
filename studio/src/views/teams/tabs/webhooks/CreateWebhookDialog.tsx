import { useState } from "react";

import {
  type WebhookConfig,
  type WebhookProvider,
  createWebhook,
} from "@/api/webhooks";
import type { BotEntryWithSchema } from "@/api/bots";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Dialog } from "@/components/ui/Dialog";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Radio } from "@/components/ui/Radio";
import { Select } from "@/components/ui/Select";
import { TagInput } from "@/components/ui/TagInput";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import {
  StepIndicator,
  type WizardStepDef,
} from "@/views/integrations/wizard/StepIndicator";

import { Field } from "./Field";

const PROVIDERS: Array<{ id: WebhookProvider; label: string; available: boolean }> = [
  { id: "gitlab", label: "GitLab", available: true },
  { id: "github", label: "GitHub", available: true },
  { id: "forgejo", label: "Forgejo / Gitea", available: true },
  { id: "generic", label: "Generic (JSON)", available: true },
];

// Event kinds each provider's inbound handler actually dispatches on —
// mirrors the webhooks.MatchEvent call sites in pkg/server:
//   gitlab  → merge_request, note, issues   (webhooks_gitlab.go)
//   github  → pull_request, issue_comment, issues (webhooks_github.go + webhooks_prforge.go)
//   forgejo → pull_request, issue_comment   (webhooks_forgejo.go)
//   generic → no event filtering (kind is always "generic")
// These seed the event allowlist when a provider is picked; an empty list
// means "provider defaults" server-side, so pre-filling is equivalent but
// explicit and editable.
const PROVIDER_EVENT_DEFAULTS: Record<WebhookProvider, string[]> = {
  gitlab: ["merge_request", "note", "issues"],
  github: ["pull_request", "issue_comment", "issues"],
  forgejo: ["pull_request", "issue_comment"],
  generic: [],
};

const STEP_IDS = ["provider", "bots", "filters", "review"] as const;
type StepId = (typeof STEP_IDS)[number];

const STEPS: WizardStepDef[] = [
  { id: "provider", label: "Provider" },
  { id: "bots", label: "Bots" },
  { id: "filters", label: "Filters & limits" },
  { id: "review", label: "Review" },
];

// Guided create-webhook wizard — same Dialog, four steps instead of one
// scroll wall. The final POST payload is unchanged: the parent receives
// the freshly-minted (config, token) pair so it can hand it off to the
// token-once panel.
export function CreateWebhookDialog({
  teamID,
  bots,
  onClose,
  onCreated,
}: {
  teamID: string;
  bots: BotEntryWithSchema[];
  onClose: () => void;
  onCreated: (r: { config: WebhookConfig; token: string }) => void;
}) {
  const [step, setStep] = useState<StepId>("provider");
  const [name, setName] = useState("");
  const [provider, setProvider] = useState<WebhookProvider>("gitlab");
  const [wildcard, setWildcard] = useState(false);
  const [botIDs, setBotIDs] = useState<string[]>([]);
  const [defaultBot, setDefaultBot] = useState("");
  const [projectAllow, setProjectAllow] = useState<string[]>([]);
  const [eventAllow, setEventAllow] = useState<string[]>(
    PROVIDER_EVENT_DEFAULTS.gitlab,
  );
  // Once the operator edits the event list by hand, switching provider
  // stops resetting it — their explicit choice wins.
  const [eventsTouched, setEventsTouched] = useState(false);
  const [forgeBaseURL, setForgeBaseURL] = useState("");
  const [rate, setRate] = useState<number>(1.0);
  const [burst, setBurst] = useState<number>(10);
  const [monthlyCap, setMonthlyCap] = useState<number>(0);
  const [launchVars, setLaunchVars] = useState<Array<{ k: string; v: string }>>([]);
  const { busy, error: err, run } = useAsyncAction();

  const pickProvider = (p: WebhookProvider) => {
    setProvider(p);
    if (!eventsTouched) setEventAllow(PROVIDER_EVENT_DEFAULTS[p]);
  };

  const limitsValid =
    [rate, burst, monthlyCap].every((n) => Number.isFinite(n) && n >= 0);

  const stepValid: Record<StepId, boolean> = {
    provider: name.trim() !== "",
    bots: wildcard || botIDs.length > 0,
    filters: limitsValid,
    review: true,
  };
  stepValid.review = stepValid.provider && stepValid.bots && stepValid.filters;

  const stepIdx = STEP_IDS.indexOf(step);
  const back = () => {
    const prev = STEP_IDS[stepIdx - 1];
    if (prev) setStep(prev);
  };
  const next = () => {
    const following = STEP_IDS[stepIdx + 1];
    if (following && stepValid[step]) setStep(following);
  };

  const submit = () =>
    run(async () => {
      const lvs = launchVars
        .filter((kv) => kv.k.trim() !== "")
        .reduce<Record<string, string>>((acc, kv) => {
          acc[kv.k.trim()] = kv.v;
          return acc;
        }, {});
      const r = await createWebhook(teamID, {
        name: name.trim(),
        provider,
        wildcard_bots: wildcard,
        bot_ids: wildcard ? undefined : botIDs,
        default_bot_id: defaultBot || undefined,
        project_allowlist: projectAllow.length ? projectAllow : undefined,
        event_allowlist: eventAllow.length ? eventAllow : undefined,
        forge_base_url: forgeBaseURL.trim() || undefined,
        rate_limit: { rate, burst },
        monthly_call_limit: monthlyCap > 0 ? monthlyCap : undefined,
        launch_vars: Object.keys(lvs).length ? lvs : undefined,
      });
      onCreated(r);
    });

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title="New inbound webhook"
      widthClass="max-w-2xl"
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          {stepIdx > 0 && (
            <Button variant="ghost" onClick={back}>
              ← Back
            </Button>
          )}
          {step !== "review" ? (
            <Button variant="primary" onClick={next} disabled={!stepValid[step]}>
              Next
            </Button>
          ) : (
            <Button
              variant="primary"
              onClick={() => void submit()}
              loading={busy}
              disabled={!stepValid.review}
            >
              Create webhook
            </Button>
          )}
        </>
      }
    >
      <div className="mb-4">
        <StepIndicator
          steps={STEPS}
          current={step}
          ariaLabel="New inbound webhook — progress"
        />
      </div>

      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-3">
          {err}
        </InlineBanner>
      )}

      <div className="space-y-3 text-sm">
        {step === "provider" && (
          <>
            <Field label="Name">
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="GitLab — review bots"
                required
                autoFocus
              />
            </Field>

            <Field label="Provider">
              <div className="flex flex-wrap gap-2">
                {PROVIDERS.map((p) => (
                  <label
                    key={p.id}
                    className={`inline-flex items-center gap-1.5 border rounded px-2 py-1 text-xs cursor-pointer ${
                      provider === p.id
                        ? "border-accent bg-accent-soft"
                        : "border-border-subtle"
                    } ${p.available ? "" : "opacity-60 cursor-not-allowed"}`}
                  >
                    <Radio
                      name="provider"
                      value={p.id}
                      checked={provider === p.id}
                      disabled={!p.available}
                      onChange={() => p.available && pickProvider(p.id)}
                    />
                    {p.label}
                  </label>
                ))}
              </div>
            </Field>

            {provider === "gitlab" && (
              <Field label="Forge base URL (optional)">
                <Input
                  value={forgeBaseURL}
                  onChange={(e) => setForgeBaseURL(e.target.value)}
                  placeholder="https://gitlab.example.com"
                />
                <div className="text-xs text-fg-subtle mt-1">
                  Pin the GitLab instance this webhook may send its bot token
                  to. A delivery whose merge-request URL host differs is
                  refused. Leave empty to derive the host from the payload.
                </div>
              </Field>
            )}
          </>
        )}

        {step === "bots" && (
          <Field label="Bot scope">
            <div className="space-y-2">
              <label className="inline-flex items-center gap-2 text-xs">
                <Checkbox
                  checked={wildcard}
                  onChange={(e) => setWildcard(e.target.checked)}
                />
                Allow any bot (wildcard — broad surface, audited as such)
              </label>
              {!wildcard && (
                <div className="space-y-1">
                  <div className="text-xs text-fg-subtle">
                    Pick the bots this webhook can launch:
                  </div>
                  <div className="flex flex-wrap gap-1 max-h-48 overflow-auto border border-border-subtle rounded p-2">
                    {bots.length === 0 ? (
                      <span className="text-xs text-fg-subtle">No bots discovered.</span>
                    ) : (
                      bots.map((b) => {
                        const checked = botIDs.includes(b.name);
                        return (
                          <label
                            key={b.name}
                            className={`inline-flex items-center gap-1 border rounded px-2 py-0.5 text-xs cursor-pointer ${
                              checked
                                ? "border-accent bg-accent-soft"
                                : "border-border-subtle"
                            }`}
                          >
                            <Checkbox
                              checked={checked}
                              onChange={() => {
                                if (checked) {
                                  setBotIDs(botIDs.filter((x) => x !== b.name));
                                  if (defaultBot === b.name) setDefaultBot("");
                                } else {
                                  setBotIDs([...botIDs, b.name]);
                                }
                              }}
                            />
                            <span>{b.display_name || b.name}</span>
                          </label>
                        );
                      })
                    )}
                  </div>
                  {botIDs.length > 1 && (
                    <Field label="Default bot (optional)" inline>
                      <Select
                        value={defaultBot}
                        onChange={(e) => setDefaultBot(e.target.value)}
                      >
                        <option value="">— pick at delivery time —</option>
                        {botIDs.map((id) => (
                          <option key={id} value={id}>
                            {id}
                          </option>
                        ))}
                      </Select>
                    </Field>
                  )}
                </div>
              )}
            </div>
          </Field>
        )}

        {step === "filters" && (
          <>
            <Field label="Project allowlist (paths)">
              <TagInput
                value={projectAllow}
                onChange={setProjectAllow}
                placeholder="group/repo"
              />
              <div className="text-xs text-fg-subtle mt-1">
                Empty = every project this token reaches.
              </div>
            </Field>

            <Field label="Event allowlist">
              <TagInput
                value={eventAllow}
                onChange={(v) => {
                  setEventsTouched(true);
                  setEventAllow(v);
                }}
                placeholder="merge_request, note, …"
              />
              <div className="text-xs text-fg-subtle mt-1">
                {provider === "generic"
                  ? "Generic deliveries are not event-filtered — leave empty."
                  : "Pre-filled with the events this provider supports; trim to gate some off."}
              </div>
            </Field>

            <div className="grid grid-cols-3 gap-2">
              <Field label="Rate (req/s)">
                <Input
                  type="number"
                  min={0}
                  step={0.1}
                  value={String(rate)}
                  onChange={(e) => setRate(Number(e.target.value))}
                />
              </Field>
              <Field label="Burst">
                <Input
                  type="number"
                  min={0}
                  value={String(burst)}
                  onChange={(e) => setBurst(Number(e.target.value))}
                />
              </Field>
              <Field label="Monthly cap (0 = inherit)">
                <Input
                  type="number"
                  min={0}
                  value={String(monthlyCap)}
                  onChange={(e) => setMonthlyCap(Number(e.target.value))}
                />
              </Field>
            </div>

            <Field label="Launch vars">
              <div className="space-y-1">
                {launchVars.map((kv, i) => (
                  <div key={i} className="flex gap-1">
                    <Input
                      placeholder="key"
                      value={kv.k}
                      onChange={(e) => {
                        const next = [...launchVars];
                        next[i] = { ...kv, k: e.target.value };
                        setLaunchVars(next);
                      }}
                    />
                    <Input
                      placeholder="value"
                      value={kv.v}
                      onChange={(e) => {
                        const next = [...launchVars];
                        next[i] = { ...kv, v: e.target.value };
                        setLaunchVars(next);
                      }}
                    />
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setLaunchVars(launchVars.filter((_, j) => j !== i))
                      }
                    >
                      ×
                    </Button>
                  </div>
                ))}
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setLaunchVars([...launchVars, { k: "", v: "" }])}
                >
                  + Add var
                </Button>
              </div>
            </Field>
          </>
        )}

        {step === "review" && (
          <ReviewSummary
            name={name}
            provider={provider}
            forgeBaseURL={forgeBaseURL}
            wildcard={wildcard}
            botIDs={botIDs}
            defaultBot={defaultBot}
            projectAllow={projectAllow}
            eventAllow={eventAllow}
            rate={rate}
            burst={burst}
            monthlyCap={monthlyCap}
            launchVars={launchVars}
          />
        )}
      </div>
    </Dialog>
  );
}

/* --------------------------- review summary -------------------------- */

function ReviewSummary({
  name,
  provider,
  forgeBaseURL,
  wildcard,
  botIDs,
  defaultBot,
  projectAllow,
  eventAllow,
  rate,
  burst,
  monthlyCap,
  launchVars,
}: {
  name: string;
  provider: WebhookProvider;
  forgeBaseURL: string;
  wildcard: boolean;
  botIDs: string[];
  defaultBot: string;
  projectAllow: string[];
  eventAllow: string[];
  rate: number;
  burst: number;
  monthlyCap: number;
  launchVars: Array<{ k: string; v: string }>;
}) {
  const providerLabel = PROVIDERS.find((p) => p.id === provider)?.label ?? provider;
  const vars = launchVars.filter((kv) => kv.k.trim() !== "");
  return (
    <div className="space-y-3">
      <div className="text-xs text-fg-subtle">
        Review the webhook before creating it — the signing token is shown once
        after creation.
      </div>
      <dl className="rounded border border-border-subtle divide-y divide-border-subtle text-xs">
        <ReviewRow label="Name">{name.trim()}</ReviewRow>
        <ReviewRow label="Provider">
          {providerLabel}
          {provider === "gitlab" && forgeBaseURL.trim() && (
            <span className="text-fg-subtle"> · pinned to {forgeBaseURL.trim()}</span>
          )}
        </ReviewRow>
        <ReviewRow label="Bots">
          {wildcard ? (
            "Any bot (wildcard)"
          ) : (
            <>
              {botIDs.join(", ")}
              {defaultBot && (
                <span className="text-fg-subtle"> · default: {defaultBot}</span>
              )}
            </>
          )}
        </ReviewRow>
        <ReviewRow label="Projects">
          {projectAllow.length ? projectAllow.join(", ") : "All projects"}
        </ReviewRow>
        <ReviewRow label="Events">
          {provider === "generic"
            ? "Not event-filtered (generic)"
            : eventAllow.length
              ? eventAllow.join(", ")
              : "Provider defaults"}
        </ReviewRow>
        <ReviewRow label="Limits">
          {rate} req/s · burst {burst} ·{" "}
          {monthlyCap > 0 ? `${monthlyCap}/month` : "monthly cap inherited"}
        </ReviewRow>
        {vars.length > 0 && (
          <ReviewRow label="Launch vars">
            {vars.map((kv) => (
              <div key={kv.k} className="font-mono">
                {kv.k.trim()}={kv.v}
              </div>
            ))}
          </ReviewRow>
        )}
      </dl>
    </div>
  );
}

function ReviewRow({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid grid-cols-[7rem_1fr] gap-2 px-3 py-2">
      <dt className="text-fg-subtle">{label}</dt>
      <dd className="text-fg-default min-w-0 break-words">{children}</dd>
    </div>
  );
}
