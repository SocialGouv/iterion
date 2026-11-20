/**
 * Detects the LLM provider from model spec or delegate field.
 *
 * Returns an icon key (matching @lobehub/icons module ID) and a display label.
 * Detection order:
 *   1. Delegate name (exact matches for known delegates)
 *   2. Model prefix before "/" (e.g. "anthropic/claude-sonnet-4" -> "anthropic")
 *   3. Model keyword/substring (e.g. "${ANTHROPIC_MODEL}" -> "anthropic")
 * A spec that starts with the gateway protocol
 * ("openai_compatible/scaleway/deepseek-v4.1-flash") skips step 2 and reaches
 * step 3 without it -> "deepseek".
 */

export interface ProviderInfo {
  /** Icon module ID in @lobehub/icons (e.g. "Claude", "OpenAI", "Gemini") */
  iconId: string;
  /** Human-readable label */
  label: string;
  /** Whether the icon has a Color variant */
  hasColor: boolean;
}

// Known delegate -> provider mapping
const DELEGATE_MAP: Record<string, ProviderInfo> = {
  claude_code: { iconId: "Claude", label: "Claude Code", hasColor: true },
  claude: { iconId: "Claude", label: "Claude", hasColor: true },
  codex: { iconId: "Codex", label: "Codex", hasColor: true },
  openai: { iconId: "OpenAI", label: "OpenAI", hasColor: false },
  // xAI Grok Build CLI (`backend: "grok"`) — coding agent, not the metered API.
  grok: { iconId: "XAI", label: "Grok", hasColor: false },
  kimi: { iconId: "OpenAI", label: "Kimi", hasColor: false },
};

// Model prefix (before "/") -> provider mapping
// Covers the format "provider/model-id"
const PREFIX_MAP: Record<string, ProviderInfo> = {
  anthropic: { iconId: "Claude", label: "Anthropic", hasColor: true },
  openai: { iconId: "OpenAI", label: "OpenAI", hasColor: false },
  "openai-codex": { iconId: "Codex", label: "Codex", hasColor: true },
  google: { iconId: "Gemini", label: "Google", hasColor: true },
  gemini: { iconId: "Gemini", label: "Gemini", hasColor: true },
  mistral: { iconId: "Mistral", label: "Mistral", hasColor: true },
  deepseek: { iconId: "DeepSeek", label: "DeepSeek", hasColor: true },
  meta: { iconId: "Meta", label: "Meta", hasColor: true },
  cohere: { iconId: "Cohere", label: "Cohere", hasColor: true },
  groq: { iconId: "Groq", label: "Groq", hasColor: false },
  xai: { iconId: "XAI", label: "xAI", hasColor: false },
  ollama: { iconId: "Ollama", label: "Ollama", hasColor: false },
  together: { iconId: "Together", label: "Together", hasColor: true },
  fireworks: { iconId: "Fireworks", label: "Fireworks", hasColor: true },
  replicate: { iconId: "Replicate", label: "Replicate", hasColor: false },
  bedrock: { iconId: "Bedrock", label: "Bedrock", hasColor: true },
  azure: { iconId: "Azure", label: "Azure", hasColor: true },
  cerebras: { iconId: "Cerebras", label: "Cerebras", hasColor: true },
  perplexity: { iconId: "Perplexity", label: "Perplexity", hasColor: true },
  nvidia: { iconId: "Nvidia", label: "NVIDIA", hasColor: true },
  sambanova: { iconId: "SambaNova", label: "SambaNova", hasColor: true },
  cloudflare: { iconId: "Cloudflare", label: "Cloudflare", hasColor: true },
  github: { iconId: "Github", label: "GitHub", hasColor: false },
  aws: { iconId: "Aws", label: "AWS", hasColor: true },
  huggingface: { iconId: "HuggingFace", label: "Hugging Face", hasColor: true },
};

// Keyword -> prefix key mapping for substring matching (env vars, bare model names)
const KEYWORD_MAP: [string, string][] = [
  ["anthropic", "anthropic"],
  ["claude", "anthropic"],
  ["openai", "openai"],
  ["gpt", "openai"],
  ["gemini", "google"],
  ["mistral", "mistral"],
  ["mixtral", "mistral"],
  ["deepseek", "deepseek"],
  // Prefer the whole provider name over its embedded "llama" substring.
  ["ollama", "ollama"],
  ["llama", "meta"],
  ["cohere", "cohere"],
  ["command-r", "cohere"],
  ["groq", "groq"],
  ["grok", "xai"],
  ["together", "together"],
  ["fireworks", "fireworks"],
  ["bedrock", "bedrock"],
  ["cerebras", "cerebras"],
  ["perplexity", "perplexity"],
  ["nvidia", "nvidia"],
  ["sambanova", "sambanova"],
  ["huggingface", "huggingface"],
  ["hf", "huggingface"],
];

// The gateway protocol's name says nothing about the vendor, wherever a model
// spec carries it: as a route's first segment
// ("openai_compatible/scaleway/deepseek-v4.1-flash"), inside an env default
// ("${M:-openai_compatible/…}"), or in the name of the variable that holds the
// route ("${OPENAI_COMPATIBLE_MODEL}"). Left in place, the "openai" keyword
// claims the model for OpenAI unless an earlier keyword matched first.
const GATEWAY_PROTOCOL = /openai_compatible\/?/g;

export function detectProvider(model?: string, delegate?: string): ProviderInfo | null {
  // 1. Check delegate (strongest signal)
  if (delegate) {
    const d = delegate.toLowerCase();
    if (DELEGATE_MAP[d]) return DELEGATE_MAP[d];
    // Prefix match for delegate variants (e.g. "claude_something")
    for (const [key, info] of Object.entries(DELEGATE_MAP)) {
      if (d.startsWith(key)) return info;
    }
  }

  if (!model) return null;
  const lower = model.toLowerCase();
  const m = lower.replace(GATEWAY_PROTOCOL, "");
  // What follows the protocol in a gateway route is the gateway's own
  // namespace ("scaleway", "azure"), not a vendor prefix: a spec starting with
  // the protocol skips step 2 and the keywords decide, so the model
  // keeps the vendor its name carries. A namespace that is itself a keyword
  // (bedrock, groq) still decides when the model's name carries none.
  // A protocol-named variable inside a vendor-prefixed spec is not a gateway.
  const viaGateway = lower.startsWith("openai_compatible/");

  // 2. Explicit prefix: "provider/model-id"
  const slashIdx = m.indexOf("/");
  if (!viaGateway && slashIdx > 0) {
    const prefix = m.slice(0, slashIdx);
    if (PREFIX_MAP[prefix]) return PREFIX_MAP[prefix];
  }

  // 3. Keyword/substring match (env vars like ${ANTHROPIC_MODEL}, bare names like "gemini-2.5-pro")
  for (const [keyword, prefixKey] of KEYWORD_MAP) {
    if (m.includes(keyword)) return PREFIX_MAP[prefixKey] ?? null;
  }

  return null;
}
