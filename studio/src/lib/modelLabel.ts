import type { FallbackDecl } from "@/api/types";

// Display helpers for agent/judge `model:` (and `fallbacks:`) on the
// studio canvas. The historical card replaced every `${...}` with the
// word "env", which hid both the authored default
// (`${VAR:-openai-codex/gpt-5.6-sol}`) and the live env value (sol vs
// terra vs luna). These functions keep the card short enough for the
// 160px node while still naming the actual spec.

const ENV_DEFAULT = /^\$\{[A-Za-z_][A-Za-z0-9_]*:-(.+)\}$/;
const ENV_BARE = /\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]+))?\}/g;

/** Last path segment of a model spec: `openai-codex/gpt-5.6-sol` → `gpt-5.6-sol`. */
export function shortenModel(spec: string): string {
  const trimmed = spec.trim();
  if (!trimmed) return trimmed;
  const slash = trimmed.lastIndexOf("/");
  if (slash >= 0 && slash < trimmed.length - 1) return trimmed.slice(slash + 1);
  return trimmed;
}

/**
 * Default of a whole-string `${VAR:-default}` form. Nested defaults
 * (`${A:-${B:-c}}`) are left to the server resolver; this only covers
 * the single-level form catalog bots actually write.
 */
export function envDefault(literal: string | undefined): string | undefined {
  if (!literal) return undefined;
  const m = literal.trim().match(ENV_DEFAULT);
  const def = m?.[1]?.trim();
  return def && !def.includes("${") ? def : undefined;
}

/** Compact `${FOO}` / mixed `prefix-${FOO}` into `$FOO` or the inline default. */
export function compactEnvRef(literal: string): string {
  return literal.replace(ENV_BARE, (_all, name: string, def?: string) =>
    def && !def.includes("${") ? def : `$${name}`,
  );
}

/**
 * Label shown on the node card.
 *
 * Priority: server-resolved spec → authored `${VAR:-default}` →
 * compact env-ref → shortened literal. Never the word "env".
 */
export function displayModel(
  literal: string | undefined,
  resolved?: string,
): string | undefined {
  const live = resolved?.trim();
  if (live) return shortenModel(live);
  if (!literal?.trim()) return undefined;
  const def = envDefault(literal);
  if (def) return shortenModel(def);
  if (literal.includes("${")) return compactEnvRef(literal);
  return shortenModel(literal);
}

/** Hover title: original literal plus the live expansion when they differ. */
export function modelTooltip(
  literal: string | undefined,
  resolved?: string,
): string | undefined {
  if (!literal?.trim() && !resolved?.trim()) return undefined;
  const live = resolved?.trim();
  if (live && literal && live !== literal.trim()) {
    return `model: ${live}\ndeclared: ${literal}`;
  }
  if (live) return `model: ${live}`;
  return literal ? `model: ${literal}` : undefined;
}

export function displayFallbackRoute(
  fb: FallbackDecl,
  resolvedModel?: string,
): string {
  const model = displayModel(fb.model, resolvedModel);
  const backend = fb.backend?.trim();
  if (backend && model) return `${backend}/${model}`;
  if (model) return model;
  if (backend) return backend;
  return fb.name;
}

export function displayFallbackChain(
  fallbacks: FallbackDecl[] | undefined,
  resolved?: Array<string | undefined>,
): string | undefined {
  if (!fallbacks || fallbacks.length === 0) return undefined;
  return fallbacks
    .map((fb, i) => displayFallbackRoute(fb, resolved?.[i]))
    .join(" → ");
}

export function fallbackTooltip(
  fallbacks: FallbackDecl[] | undefined,
  resolved?: Array<string | undefined>,
): string | undefined {
  if (!fallbacks || fallbacks.length === 0) return undefined;
  return fallbacks
    .map((fb, i) => {
      const parts = [fb.name];
      if (fb.backend) parts.push(`backend ${fb.backend}`);
      const live = resolved?.[i]?.trim() || fb.model;
      if (live) parts.push(`model ${live}`);
      if (fb.on && fb.on.length > 0) parts.push(`on ${fb.on.join(",")}`);
      if (fb.metered) parts.push("metered");
      return parts.join(" · ");
    })
    .join("\n");
}
