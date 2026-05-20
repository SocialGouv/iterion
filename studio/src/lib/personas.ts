/**
 * Curated bot personas displayed in the chat UI in place of "AI".
 * Each value is a lightly modified form of the bot's technical name
 * (kebab-case key) — recognizable at a glance while reading as a name.
 *
 * Consumed today only by the WhatsNext chat bubble. When the chat
 * generalizes to all runs, the consumer will look up the running
 * bot's name in this map. Bots without an entry will get a derived
 * fallback (implementation deferred to that generalization).
 */
export const BOT_PERSONAS: Record<string, string> = {
  "whats-next": "Nexie",
  fix: "Fixie",
  review: "Revvy",
  "roadmap-synthesis": "Roadie",
  "branch-improve-loop": "Bily",
  "whole-improve-loop": "Wholy",
  "feature-dev": "Featurly",
};
