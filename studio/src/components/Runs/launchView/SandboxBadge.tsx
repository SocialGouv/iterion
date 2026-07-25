// Extracted from LaunchView.tsx to keep that file focused.
// SandboxBadge surfaces the workflow's sandbox isolation level next to
// the Launch button so the operator never confirms a host-execution run
// by accident. Sandboxing is the engine default, so:
//   auto / inline    → green "sandboxed" pill (declared)
//   auto (default)   → green pill (no block — engine default applies)
//   none             → red "host execution" pill (explicit opt-out)
// The badge title carries the long-form description so the chip itself
// stays compact in the Launch row.

import { CheckCircledIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";

export default function SandboxBadge({ mode }: { mode: string }) {
  const active = mode !== "none";
  const label = active ? `Sandbox: ${mode || "auto (default)"}` : "Sandbox: none";
  const cls = active
    ? "bg-success-soft text-success-fg border-success/40"
    : "bg-danger-soft text-danger-fg border-danger/40";
  const title = active
    ? mode === "auto (default)"
      ? "No sandbox block declared — the engine default applies: tools run inside a container (devcontainer-aware, falling back to the default image)."
      : "Workflow declares a sandbox block — tools run inside the container."
    : "This workflow explicitly opts out of sandboxing (sandbox: none): tools and shell commands run directly on the host. Remove the block to run sandboxed (the default).";
  return (
    <span
      className={`inline-flex items-center gap-1 text-caption px-1.5 py-0.5 rounded border ${cls}`}
      title={title}
    >
      {active ? (
        <CheckCircledIcon className="w-3 h-3" aria-hidden="true" />
      ) : (
        <ExclamationTriangleIcon className="w-3 h-3" aria-hidden="true" />
      )}
      {label}
    </span>
  );
}
