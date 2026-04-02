/**
 * Detects the LLM provider from model spec or delegate field.
 *
 * Returns an icon key (matching @lobehub/icons module ID) and a display label.
 * Detection order:
 *   1. Delegate name (exact matches for known delegates)
 *   2. Model prefix before "/" (e.g. "anthropic/claude-sonnet-4" -> "anthropic")
 *   3. Model keyword/substring (e.g. "${ANTHROPIC_MODEL}" -> "anthropic")
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
};

// Model prefix (before "/") -> provider mapping
// Covers the format "provider/model-id"
const PREFIX_MAP: Record<string, ProviderInfo> = {
  anthropic: { iconId: "Claude", label: "Anthropic", hasColor: true },
  openai: { iconId: "OpenAI", label: "OpenAI", hasColor: false },
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
  ["llama", "meta"],
  ["cohere", "cohere"],
  ["command-r", "cohere"],
  ["groq", "groq"],
  ["grok", "xai"],
  ["ollama", "ollama"],
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
  const m = model.toLowerCase();

  // 2. Explicit prefix: "provider/model-id"
  const slashIdx = m.indexOf("/");
  if (slashIdx > 0) {
    const prefix = m.slice(0, slashIdx);
    if (PREFIX_MAP[prefix]) return PREFIX_MAP[prefix];
  }

  // 3. Keyword/substring match (env vars like ${ANTHROPIC_MODEL}, bare names like "gemini-2.5-pro")
  for (const [keyword, prefixKey] of KEYWORD_MAP) {
    if (m.includes(keyword)) return PREFIX_MAP[prefixKey] ?? null;
  }

  return null;
}
