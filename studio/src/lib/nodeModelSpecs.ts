// nodeModelSpecs collects the model specs a bot's LLM nodes pin in their DSL,
// so the launch form can ask the model registry about them alongside the
// curated set. A bot pinned to something the curated list omits would
// otherwise render as an unknown value in the picker.

export interface SpecBearingNode {
  model: string;
}

// A ${VAR} literal is a template placeholder resolved at launch, not a model
// id. Asking the registry about it yields a confident answer about a model
// that does not exist, so it is dropped rather than passed through.
export function nodeModelSpecs(nodes: SpecBearingNode[]): string[] {
  const set = new Set<string>();
  for (const n of nodes) {
    const m = n.model?.trim();
    if (m && !m.includes("${")) set.add(m);
  }
  return [...set].sort();
}
