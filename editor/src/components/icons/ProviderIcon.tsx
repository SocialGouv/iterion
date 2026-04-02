import type { ComponentType } from "react";
import { detectProvider } from "./providerDetect";

import Claude from "@lobehub/icons/es/Claude";
import Codex from "@lobehub/icons/es/Codex";
import OpenAI from "@lobehub/icons/es/OpenAI";
import Gemini from "@lobehub/icons/es/Gemini";
import Mistral from "@lobehub/icons/es/Mistral";
import DeepSeek from "@lobehub/icons/es/DeepSeek";
import Meta from "@lobehub/icons/es/Meta";
import Cohere from "@lobehub/icons/es/Cohere";
import Groq from "@lobehub/icons/es/Groq";
import XAI from "@lobehub/icons/es/XAI";
import Ollama from "@lobehub/icons/es/Ollama";
import Together from "@lobehub/icons/es/Together";
import Fireworks from "@lobehub/icons/es/Fireworks";
import Replicate from "@lobehub/icons/es/Replicate";
import Bedrock from "@lobehub/icons/es/Bedrock";
import Azure from "@lobehub/icons/es/Azure";
import Cerebras from "@lobehub/icons/es/Cerebras";
import Perplexity from "@lobehub/icons/es/Perplexity";
import Nvidia from "@lobehub/icons/es/Nvidia";
import SambaNova from "@lobehub/icons/es/SambaNova";
import Cloudflare from "@lobehub/icons/es/Cloudflare";
import Github from "@lobehub/icons/es/Github";
import Aws from "@lobehub/icons/es/Aws";
import HuggingFace from "@lobehub/icons/es/HuggingFace";

type IconComp = ComponentType<{ size?: number; className?: string }>;

// Map iconId -> component (Color variant preferred when available)
const ICONS: Record<string, IconComp> = {
  Claude: Claude.Color,
  Codex: Codex.Color,
  OpenAI: OpenAI,
  Gemini: Gemini.Color,
  Mistral: Mistral.Color,
  DeepSeek: DeepSeek.Color,
  Meta: Meta,
  Cohere: Cohere.Color,
  Groq: Groq,
  XAI: XAI,
  Ollama: Ollama,
  Together: Together.Color,
  Fireworks: Fireworks.Color,
  Replicate: Replicate,
  Bedrock: Bedrock.Color,
  Azure: Azure.Color,
  Cerebras: Cerebras.Color,
  Perplexity: Perplexity.Color,
  Nvidia: Nvidia.Color,
  SambaNova: SambaNova.Color,
  Cloudflare: Cloudflare.Color,
  Github: Github,
  Aws: Aws.Color,
  HuggingFace: HuggingFace.Color,
};

interface Props {
  model?: string;
  delegate?: string;
  size?: number;
  className?: string;
}

export function ProviderIcon({ model, delegate, size = 12, className }: Props) {
  const info = detectProvider(model, delegate);
  if (!info) return null;
  const Icon = ICONS[info.iconId];
  if (!Icon) return null;
  return <Icon size={size} className={className} />;
}

export function ProviderLabel({ model, delegate }: { model?: string; delegate?: string }): string | null {
  const info = detectProvider(model, delegate);
  return info?.label ?? null;
}
