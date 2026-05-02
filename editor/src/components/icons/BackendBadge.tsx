// Visual badge for the execution backend (claw / claude_code / codex).
// Uses brand SVGs for delegates that have one (claude_code → Claude,
// codex → Codex) and a crab emoji for claw, iterion's in-process
// backend. Empty/undefined backend renders as claw because that's the
// runtime default — see pkg/backend/model/executor.go:resolveBackendName,
// which falls back to BackendClaw when no backend is set.

import Claude from "@lobehub/icons/es/Claude";
import Codex from "@lobehub/icons/es/Codex";

import { backendEmoji } from "./backendEmoji";

interface Props {
  // Raw backend value from the .iter source. Empty/undefined → "claw".
  backend?: string;
  size?: number;
  showLabel?: boolean;
  className?: string;
}

// Resolve the displayed backend name. The runtime treats an empty value
// as the workflow default (which itself defaults to "claw"), so the
// editor surfaces the same fact rather than the legacy "direct" label.
export function effectiveBackend(backend?: string): string {
  return backend && backend !== "" ? backend : "claw";
}

export function BackendBadge({
  backend,
  size = 10,
  showLabel = true,
  className,
}: Props) {
  const effective = effectiveBackend(backend);
  const isImplicit = !backend || backend === "";

  let icon;
  if (effective === "claude_code") {
    icon = <Claude.Color size={size} className="shrink-0" />;
  } else if (effective === "codex") {
    icon = <Codex.Color size={size} className="shrink-0" />;
  } else {
    icon = (
      <span
        aria-hidden
        className="shrink-0 leading-none"
        style={{ fontSize: `${size}px` }}
      >
        {backendEmoji(effective)}
      </span>
    );
  }

  return (
    <span
      className={`inline-flex items-center gap-0.5 ${className ?? ""}`}
      title={
        isImplicit
          ? "backend: claw (runtime default for unset backend)"
          : `backend: ${effective}`
      }
    >
      {icon}
      {showLabel && (
        <span className="truncate">
          {effective}
          {isImplicit && (
            <span className="ml-0.5 text-fg-subtle/70 text-[8px]">·default</span>
          )}
        </span>
      )}
    </span>
  );
}
